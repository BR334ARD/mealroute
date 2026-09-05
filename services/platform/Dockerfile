FROM golang:1.25-alpine AS build

WORKDIR /src
COPY services/platform/go.mod services/platform/go.sum ./
RUN go mod download
COPY services/platform ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/platform ./cmd/server

FROM alpine:3.22

RUN addgroup -S app && adduser -S app -G app
COPY --from=build /out/platform /usr/local/bin/platform
USER app
ENTRYPOINT ["/usr/local/bin/platform"]
