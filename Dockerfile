FROM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o curity-operator ./cmd/manager/

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.7

ARG VERSION
ARG RELEASE=1

LABEL name="Curity Operator" \
      vendor="Curity" \
      version="${VERSION}" \
      release="${RELEASE}" \
      summary="Kubernetes operator for Curity Identity Server" \
      description="Manages the lifecycle of Curity Identity Server deployments on Kubernetes"

COPY --from=builder /workspace/curity-operator /usr/local/bin/curity-operator

USER 1001
ENTRYPOINT ["curity-operator"]
