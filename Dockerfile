# Build a static binary, then ship only the binary and the CA bundle.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ghome .

FROM scratch

# Needed to reach a Gitea instance over HTTPS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ghome /ghome

# Runs unprivileged: nothing here needs root.
USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/ghome"]

# Example:
#   docker run -d --name ghome -p 8080:8080 \
#     -e GHOME_SOURCE=gitea \
#     -e GHOME_GITEA_URL=https://git.example.com \
#     -e GHOME_GITEA_OWNER=simon -e GHOME_GITEA_REPO=nav \
#     -e GHOME_GITEA_TOKEN=... -e GHOME_WEBHOOK_SECRET=... \
#     ghcr.io/simon/ghome
