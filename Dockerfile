FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY collector/go.mod collector/go.sum ./
RUN go mod download
COPY collector/*.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /collector .

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/Architects-that-code/oci-compute-inventory"
LABEL org.opencontainers.image.description="Global OCI running Compute inventory for Prometheus"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /collector /collector
USER 65532:65532
EXPOSE 9108
ENTRYPOINT ["/collector"]
