# syntax=docker/dockerfile:1
#
#   docker build -t go-neon-simple .                         # native arch
#   docker buildx build --platform linux/amd64,linux/arm64 -t go-neon-simple .
#   docker run --rm -p 8080:8080 go-neon-simple
#
# The builder runs on the host arch and cross-compiles for TARGETARCH (pure Go,
# no cgo), so building the other arch needs no QEMU. neon_arm64.s is assembled
# only when TARGETARCH=arm64; the amd64 image serves std and scalar only.

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go *.s ./
ARG TARGETOS TARGETARCH
# Tests run on the build host (correctness of the code, not of the target image).
RUN go vet ./... && go test ./...
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/server .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
ENV ADDR=:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
