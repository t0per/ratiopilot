# Rôle : construit l'image finale (binaire seul, ~12 Mo) et l'image du tracker de test.
# ---------- build ----------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=1.0.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ratiopilot . \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/testtracker ./cmd/testtracker \
 && mkdir -p /out/data /out/seed \
 && apk add --no-cache ca-certificates tzdata

# ---------- tracker de test (environnement de test uniquement) ----------
FROM scratch AS testtracker
COPY --from=build /out/testtracker /testtracker
COPY --from=build --chown=1000:1000 /out/seed /seed
USER 1000:1000
EXPOSE 6969/tcp 6969/udp
ENTRYPOINT ["/testtracker"]

# ---------- image finale : binaire seul, ~12 Mo ----------
FROM scratch AS ratiopilot
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/ratiopilot /ratiopilot
COPY --from=build --chown=1000:1000 /out/data /data
ENV RP_DATA=/data RP_LISTEN=:8080 TZ=Europe/Paris
USER 1000:1000
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/ratiopilot", "healthcheck"]
ENTRYPOINT ["/ratiopilot"]
