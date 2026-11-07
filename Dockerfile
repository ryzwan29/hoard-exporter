FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/hoard-exporter .

FROM scratch
# CA certs only matter if HOARD_URL ever points at an https:// endpoint
# (e.g. the exporter running outside the LAN, behind a reverse proxy).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hoard-exporter /hoard-exporter
EXPOSE 9477
ENTRYPOINT ["/hoard-exporter"]
