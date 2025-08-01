//go:build go1.24
// +build go1.24

// registry-gc-listener:
//   1.  Web-hook  /events  →  запускает   registry garbage-collect
//   2.  Периодически (или ручным /prune) оставляет N последних тегов
//       и затем тоже запускает garbage-collect.
//   Требование basic-auth есть только для prune; GC работает с томом данных.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/sirupsen/logrus"
)

const (
	liveCfg = "/etc/docker/registry/config.yml" // путь до конфига Registry
)

var (
	// --- ENV -----------------------------------------------------------------
	registryURL   = getenv("REGISTRY_URL", "http://registry-server:5000")
	keepN, _      = strconv.Atoi(getenv("KEEP_N", "10")) // сколько тегов оставлять
	pruneEvery, _ = time.ParseDuration(getenv("PRUNE_INTERVAL", "24h"))
	workers, _    = strconv.Atoi(getenv("WORKERS", "8"))
	user          = os.Getenv("REGISTRY_USER") // basic-auth для prune (не нужен для GC)
	pass          = os.Getenv("REGISTRY_PASS")
	debounce      = time.Minute // задержка после DELETE веб-хука
	logLevel      = getenv("LOG_LEVEL", "info")

	// --- STATE ---------------------------------------------------------------
	log      = logrus.New()
	ctx, cl  = context.WithCancel(context.Background())
	muHook   sync.Mutex
	hookPend bool // GC уже запланирован debounce-таймером?
)

// ---   TYPES   --------------------------------------------------------------

type envelope struct {
	Events []struct {
		Action string `json:"action"`
	} `json:"events"`
}

// ---   MAIN   ---------------------------------------------------------------

func main() {
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	if lvl, err := logrus.ParseLevel(strings.ToLower(logLevel)); err == nil {
		log.SetLevel(lvl)
	} else {
		log.SetLevel(logrus.InfoLevel)
	}

	// graceful-shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Info("shutdown requested")
		cl()
	}()

	// HTTP
	http.HandleFunc("/events", hookHandler) // триггер GC
	http.HandleFunc("/gc", handleGC)        // ручной GC
	http.HandleFunc("/prune", handlePrune)  // ручной prune+GC

	go func() {
		log.Info("listener on :8080")
		if err := http.ListenAndServe(":8080", nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	// периодический prune
	if pruneEvery > 0 {
		go func() {
			t := time.NewTicker(pruneEvery)
			for {
				select {
				case <-t.C:
					runPrune(ctx)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	<-ctx.Done()
}

// ---   WEB-HOOK GC   --------------------------------------------------------

func hookHandler(w http.ResponseWriter, r *http.Request) {
	var env envelope
	if json.NewDecoder(r.Body).Decode(&env) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, e := range env.Events {
		if e.Action == "delete" {
			scheduleGC()
			break
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func scheduleGC() {
	muHook.Lock()
	if hookPend {
		muHook.Unlock()
		return
	}
	hookPend = true
	muHook.Unlock()

	log.Infof("[hook] GC in %s", debounce)
	go func() {
		select {
		case <-time.After(debounce):
			runGC(ctx)
		case <-ctx.Done():
		}
		muHook.Lock()
		hookPend = false
		muHook.Unlock()
	}()
}

// ---   HANDLERS   -----------------------------------------------------------

func handleGC(w http.ResponseWriter, _ *http.Request) {
	go runGC(ctx)
	w.Write([]byte("GC started"))
}

func handlePrune(w http.ResponseWriter, _ *http.Request) {
	go runPrune(ctx)
	w.Write([]byte("prune+GC started"))
}

// ---   GC   -----------------------------------------------------------------

func runGC(c context.Context) {
	log.Info("GC start")
	cmd := exec.CommandContext(c, "registry", "garbage-collect", "--delete-untagged", liveCfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Errorf("GC error: %v", err)
		if len(out) > 0 {
			log.Debugf("GC output:\n%s", string(out))
		}
		return
	}
	log.Info("GC done")
	if log.IsLevelEnabled(logrus.DebugLevel) && len(out) > 0 {
		log.Debugf("GC output:\n%s", string(out))
	}
}

// ---   PRUNE + GC   ---------------------------------------------------------

func runPrune(c context.Context) {
	if keepN <= 0 {
		log.Info("prune disabled (KEEP_N<=0)")
		runGC(c)
		return
	}
	if user == "" {
		log.Warn("prune skipped: REGISTRY_USER/PASS not set")
		runGC(c)
		return
	}

	start := time.Now()
	if err := pruneTags(c); err != nil {
		log.Warnf("prune: %v", err)
	}
	runGC(c)
	log.Infof("prune+GC finished in %s", time.Since(start))
}

// ---   TAG CLEANER (HTTP API)   ---------------------------------------------

func pruneTags(c context.Context) error {
	repos, err := catalog(c)
	if err != nil {
		return err
	}
	log.Infof("catalog: %d repos", len(repos))

	type job struct{ repo, tag, digest string }
	jobs := make(chan job)
	wg := sync.WaitGroup{}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := deleteManifest(c, j.repo, j.digest); err != nil {
					log.Warnf("[%s:%s] delete: %v", j.repo, j.tag, err)
				} else {
					log.Debugf("[%s:%s] deleted", j.repo, j.tag)
				}
			}
		}()
	}

	for _, repo := range repos {
		tags, err := tagsList(c, repo)
		if err != nil {
			log.Warnf("[%s] tags: %v", repo, err)
			continue
		}
		if len(tags) <= keepN {
			continue
		}
		sort.Slice(tags, func(i, j int) bool { return newer(tags[i], tags[j]) })
		for _, tag := range tags[keepN:] {
			digest, err := manifestDigest(c, repo, tag)
			if err != nil {
				log.Warnf("[%s:%s] digest: %v", repo, tag, err)
				continue
			}
			jobs <- job{repo, tag, digest}
		}
	}
	close(jobs)
	wg.Wait()
	return nil
}

// ---   REGISTRY HTTP HELPERS   ----------------------------------------------

var httpClient = &http.Client{Timeout: 15 * time.Second}

func req(ctx context.Context, method, path string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, registryURL+path, nil)
	if err != nil {
		return nil, err
	}
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	return r, nil
}

func jsonDo(r *http.Request, out any) error {
	res, err := httpClient.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func catalog(ctx context.Context) ([]string, error) {
	r, _ := req(ctx, "GET", "/v2/_catalog?n=10000")
	var resp struct {
		Repositories []string `json:"repositories"`
	}
	if err := jsonDo(r, &resp); err != nil {
		return nil, err
	}
	return resp.Repositories, nil
}

func tagsList(ctx context.Context, repo string) ([]string, error) {
	r, _ := req(ctx, "GET", fmt.Sprintf("/v2/%s/tags/list", repo))
	var resp struct {
		Tags []string `json:"tags"`
	}
	if err := jsonDo(r, &resp); err != nil {
		return nil, err
	}
	return resp.Tags, nil
}

func manifestDigest(ctx context.Context, repo, tag string) (string, error) {
	r, _ := req(ctx, "GET", fmt.Sprintf("/v2/%s/manifests/%s", repo, tag))
	r.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	res, err := httpClient.Do(r)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", res.StatusCode)
	}
	return res.Header.Get("Docker-Content-Digest"), nil
}

func deleteManifest(ctx context.Context, repo, digest string) error {
	r, _ := req(ctx, "DELETE", fmt.Sprintf("/v2/%s/manifests/%s", repo, digest))
	res, err := httpClient.Do(r)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return nil
}

// ---   UTILS   --------------------------------------------------------------

func newer(a, b string) bool {
	va, ea := semver.StrictNewVersion(a)
	vb, eb := semver.StrictNewVersion(b)
	if ea == nil && eb == nil {
		return va.GreaterThan(vb)
	}
	return a > b // fallback lexicographic
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
