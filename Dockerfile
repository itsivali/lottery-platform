# syntax=docker/dockerfile:1.7
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGET
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./${TARGET}

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]

FROM runtime AS api
EXPOSE 8080
FROM runtime AS worker
FROM runtime AS lotteryctl
FROM runtime AS mock
EXPOSE 8080
