FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=384MiB go build -trimpath -o /out/tetherd ./cmd/tetherd

FROM debian:bookworm-slim
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tetherd /usr/local/bin/tetherd
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/tetherd"]
CMD ["-listen", "127.0.0.1:8090", "-data-dir", "/data"]
