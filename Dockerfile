# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nsrl-server ./cmd/nsrl-server \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nsrl-download ./cmd/nsrl-download \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nsrl-server /usr/local/bin/nsrl-server
COPY --from=build /out/nsrl-download /usr/local/bin/nsrl-download
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/nsrl-server"]
