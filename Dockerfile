FROM golang:1.18 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY controllers/ controllers/
COPY uninstall/ uninstall/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM quay.io/rawagner/rosa-img:latest
WORKDIR /
COPY --from=builder /workspace/manager .
RUN mkdir -p /.config
RUN chown 65532:65532 /.config
RUN mkdir -p /.config/ocm
RUN chown 65532:65532 /.config/ocm
RUN mkdir -p /.kube
RUN chown 65532:65532 /.kube
USER 65532:65532

ENTRYPOINT ["/manager"]