FROM golang:1.27.2-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/teleadmin ./cmd/teleadmin

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
  && addgroup -S teleadmin \
  && adduser -S -G teleadmin teleadmin \
  && mkdir -p /app/data \
  && chown -R teleadmin:teleadmin /app

WORKDIR /app
COPY --from=build /out/teleadmin /usr/local/bin/teleadmin

USER teleadmin
ENTRYPOINT ["teleadmin"]
CMD ["-config", "/app/config.yaml"]
