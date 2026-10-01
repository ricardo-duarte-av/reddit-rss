# Build on the runner's own architecture and cross-compile, so multi-arch
# images do not need emulation.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /reddit-rss .

# distroless/static has CA certificates and runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /reddit-rss /reddit-rss
ENV LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/reddit-rss"]
