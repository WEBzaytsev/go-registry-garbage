# Registry-GC Service

[English](README.md) | **Русский**

Микросервис для автоматической очистки Docker Registry. Сервис предоставляет несколько стратегий очистки:
- **Webhook-триггеры GC**: Автоматически запускает garbage collection после удаления манифестов
- **Периодическая очистка тегов**: Удаляет старые теги по расписанию, оставляя только последние N версий
- **Ручные триггеры**: Очистка по требованию через HTTP API

## Возможности

- 🔄 **Webhook-триггеры GC**: Автоматически запускает garbage collection после удаления манифестов
- 📅 **Периодическая очистка тегов**: Запланированная очистка с настраиваемым интервалом (по умолчанию: 24ч)
- 🏷️ **Умное управление тегами**: Сохраняет N последних тегов, удаляет старые с учетом semver
- ⏱️ **Debounce**: Группирует множественные события удаления в один запуск GC (задержка 1 минута)
- 🔧 **Множественные триггеры**: Ручной GC, ручной prune+GC и webhook эндпоинты
- 🔐 **Селективная аутентификация**: Basic Auth требуется только для очистки тегов, не для GC
- ⚡ **Параллельная обработка**: Настраиваемое количество воркеров для одновременного удаления тегов
- 🛡️ **Graceful shutdown**: Правильная обработка сигналов и отмена контекста

## Быстрый старт

### 1. Сборка образа

```bash
# Сборка локально
docker build -t registry-gc-listener:latest .

# Или использование готового образа из registry
docker pull r.zaitsv.dev/go-registry-garbage:latest
```

### 2. Docker Compose конфигурация

```yaml
version: '3.8'

services:
  registry-ui:
    image: joxit/docker-registry-ui:main
    environment:
      - SINGLE_REGISTRY=true
      - REGISTRY_TITLE=My Registry
      - DELETE_IMAGES=true
      - SHOW_CONTENT_DIGEST=true
      - NGINX_PROXY_PASS_URL=http://registry-server:5000
      - SHOW_CATALOG_NB_TAGS=true
      - CATALOG_MIN_BRANCHES=1
      - CATALOG_MAX_BRANCHES=1
      - TAGLIST_PAGE_SIZE=100
      - REGISTRY_SECURED=false
      - CATALOG_ELEMENTS_LIMIT=1000
    networks: [registry-net]

  registry-server:
    image: registry:2.8.2
    environment:
      REGISTRY_STORAGE_DELETE_ENABLED: "true"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Origin: "[http://registry-ui]"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Methods: "[HEAD,GET,OPTIONS,DELETE]"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Credentials: "[true]"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Headers: "[Authorization,Accept,Cache-Control]"
      REGISTRY_HTTP_HEADERS_Access-Control-Expose-Headers: "[Docker-Content-Digest]"
    volumes:
      - ./registry-data:/var/lib/registry
      - ./registry-config/config.yml:/etc/docker/registry/config.yml:ro
    networks: [registry-net]

  registry-gc:
    image: r.zaitsv.dev/go-registry-garbage:latest
    depends_on: [registry-server]
    environment:
      - REGISTRY_URL=http://registry-server:5000
      - KEEP_N=10
      - WORKERS=8
      # - REGISTRY_USER=admin
      # - REGISTRY_PASS=password
    volumes:
      - ./registry-data:/var/lib/registry
      - ./registry-config/config.yml:/etc/docker/registry/config.yml:ro
    ports:
      - "8080:8080"
    networks: [registry-net]

networks:
  registry-net:
    driver: bridge
```

### 3. Конфигурация Registry

Создайте файл `registry-config/config.yml`:

```yaml
version: 0.1
log:
  fields:
    service: registry

storage:
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: true  # Обязательно для работы GC

http:
  addr: :5000
  headers:
    X-Content-Type-Options: [nosniff]

health:
  storagedriver:
    enabled: true
    interval: 10s
    threshold: 3

notifications:
  endpoints:
    - name: gc-listener
      url: http://registry-gc:8080/events
      timeout: 500ms
      threshold: 3
      backoff: 1s
```

## HTTP API

| Метод | Путь      | Описание                    | Ответ              | Требует авторизацию |
|-------|-----------|-----------------------------|--------------------|---------------------|
| POST  | `/events` | Webhook от Docker Registry  | `202 Accepted`     | Нет                 |
| POST  | `/gc`     | Ручной запуск GC           | `GC started`       | Нет                 |
| POST  | `/prune`  | Ручной запуск prune+GC     | `prune+GC started` | Да*                 |

*Аутентификация требуется только для эндпоинта `/prune`, когда установлены `REGISTRY_USER`/`REGISTRY_PASS`.

### Примеры использования

```bash
# Ручной запуск GC (авторизация не требуется)
curl -X POST http://localhost:8080/gc

# Ручной запуск prune+GC (требует авторизацию, если настроена)
curl -X POST http://localhost:8080/prune

# Отправка тестового webhook события
curl -X POST http://localhost:8080/events \
  -H "Content-Type: application/json" \
  -d '{"events":[{"action":"delete"}]}'

# Проверка логов
docker logs registry-gc
```

## Переменные окружения

| Переменная      | По умолчанию                   | Описание                                    |
|-----------------|--------------------------------|---------------------------------------------|
| `REGISTRY_URL`  | `http://registry-server:5000`  | URL Docker Registry для API вызовов. Должен быть доступен из контейнера GC сервиса |
| `KEEP_N`        | `10`                           | Количество последних тегов для сохранения в каждом репозитории. Установите 0 для отключения очистки тегов |
| `PRUNE_INTERVAL`| `24h`                          | Как часто запускать автоматическую очистку тегов. Установите `0` для отключения периодической очистки |
| `WORKERS`       | `8`                            | Количество параллельных воркеров для удаления тегов. Больше = быстрее, но выше нагрузка |
| `REGISTRY_USER` | -                              | Имя пользователя для Basic Auth. Требуется только для операций очистки тегов (опционально) |
| `REGISTRY_PASS` | -                              | Пароль для Basic Auth. Требуется только для операций очистки тегов (опционально) |
| `LOG_LEVEL`     | `info`                         | Уровень логирования: `debug`, `info`, `warn`, `error`. Вывод команды GC и сообщения об удалении тегов показываются только при `debug` |

### Подробности о переменных

- **`REGISTRY_URL`**: Сервис должен взаимодействовать с API Docker Registry для:
  - Получения списка репозиториев (`/v2/_catalog`)
  - Получения тегов каждого репозитория (`/v2/{repo}/tags/list`)
  - Получения дайджестов манифестов (`/v2/{repo}/manifests/{tag}`)
  - Удаления манифестов (`DELETE /v2/{repo}/manifests/{digest}`)

- **`KEEP_N`**: Управляет политикой хранения тегов. Например:
  - `KEEP_N=5` сохраняет только 5 новейших тегов в репозитории
  - `KEEP_N=1` сохраняет только последний тег (агрессивная очистка)
  - `KEEP_N=0` отключает очистку тегов (только garbage collection)

- **`WORKERS`**: Балансирует скорость очистки и нагрузку на систему:
  - Больше воркеров = быстрее параллельная обработка
  - Слишком много воркеров может перегрузить реестр
  - Рекомендуется: 4-16 в зависимости от производительности реестра

- **Аутентификация**: Требуется только если в реестре включена аутентификация:
  - Установите и `REGISTRY_USER`, и `REGISTRY_PASS` для Basic Auth
  - Оставьте пустыми для публичных реестров

### Захардкоженные константы

Некоторые параметры в настоящее время захардкожены в исходном коде и требуют пересборки для изменения:

- **Время debounce**: `1 минута` - задержка после последнего события удаления перед запуском GC
- **Путь к конфигу реестра**: `/etc/docker/registry/config.yml` - путь к файлу конфигурации реестра
- **HTTP таймаут**: `10 секунд` - таймаут для вызовов API реестра
- **Порт сервера**: `:8080` - порт для webhook и ручного запуска

Для изменения этих значений отредактируйте `main.go` и пересоберите Docker образ.

## Как это работает

### Webhook-триггеры GC
1. **Webhook события**: Registry отправляет события на `/events` при удалении манифестов
2. **Debounce**: Сервис ждет 1 минуту после последнего события удаления
3. **Garbage Collection**: Запускает `registry garbage-collect --delete-untagged`

### Периодическая очистка тегов
1. **Расписание**: Запускается каждые `PRUNE_INTERVAL` (по умолчанию: 24ч)
2. **Очистка тегов**: Удаляет старые теги, оставляя только последние `KEEP_N` версий
3. **Garbage Collection**: Запускает GC после очистки тегов
4. **Аутентификация**: Требует `REGISTRY_USER`/`REGISTRY_PASS` для доступа к API

### Ручные триггеры
- **`/gc`**: Немедленный garbage collection (авторизация не требуется)
- **`/prune`**: Очистка тегов + garbage collection (требует авторизацию, если настроена)

## Алгоритм сортировки тегов

Сервис использует умную сортировку тегов:
- Если тег соответствует семантическому версионированию (semver) - сортирует по версиям
- Иначе - сортирует лексикографически
- Сохраняет самые новые теги, удаляет старые

## Логи

Примеры логов сервиса:

```
INFO[2024-01-15T10:30:00Z] listener started on :8080
INFO[2024-01-15T10:35:00Z] GC scheduled in 1m0s
INFO[2024-01-15T10:36:00Z] ==> prune & GC start
INFO[2024-01-15T10:36:00Z] catalog: 5 repos
INFO[2024-01-15T10:36:01Z] [myapp:v1.0.0] deleted
INFO[2024-01-15T10:36:02Z] [myapp:v1.0.1] deleted
INFO[2024-01-15T10:36:05Z] ==> done in 5.2s
```

## Требования

- Docker Registry 2.8.2+
- Go 1.24+ (для сборки)
- Включенное логическое удаление в Registry (`storage.delete.enabled: true`)

## Лицензия

MIT License
