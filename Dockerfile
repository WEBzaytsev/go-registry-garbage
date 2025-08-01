# ---------- build stage ----------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN go build -o /gc-listener .

# ---------- runtime stage ----------
# берем официальный образ реестра
FROM registry:2.8.2
COPY --from=build /gc-listener /usr/local/bin/gc-listener
ENTRYPOINT ["gc-listener"]
