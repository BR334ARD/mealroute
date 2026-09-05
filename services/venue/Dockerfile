FROM golang:1.25-alpine AS build

WORKDIR /src
COPY services/venue/go.mod services/venue/go.sum ./
RUN go mod download
COPY services/venue ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/venue ./cmd/server

FROM alpine:3.22

RUN addgroup -S app && adduser -S app -G app
COPY --from=build /out/venue /usr/local/bin/venue
USER app
ENTRYPOINT ["/usr/local/bin/venue"]
