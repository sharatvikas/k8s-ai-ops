FROM golang:1.23-alpine AS builder

WORKDIR /app

# Cache dependencies layer separately
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build both binaries from the same image
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /k8sai ./cmd/k8sai
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /operator ./cmd/operator

# ── CLI image ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12 AS cli

COPY --from=builder /k8sai /k8sai

USER nonroot:nonroot
ENTRYPOINT ["/k8sai"]

# ── Operator image ───────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12 AS operator

COPY --from=builder /operator /operator

USER nonroot:nonroot
ENTRYPOINT ["/operator"]
