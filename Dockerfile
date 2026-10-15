FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/hubp ./cmd/hubp \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hubp /usr/local/bin/hubp
COPY --chown=65532:65532 --from=build /out/data /data
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hubp"]
CMD ["-config", "/etc/hubp/config.json"]
