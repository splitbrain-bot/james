# Build the static binary. The web assets are embedded, so the runtime image
# needs nothing but the binary itself.
FROM golang:1.26-alpine AS build

RUN apk add --no-cache tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /james .

FROM scratch

# Certificates for the LLM API and the fetch tool, zone data for the log
# timestamps, which follow the TZ variable.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /james /james

# A deployment that reads files or a database of its application overrides this
# with the user that owns them.
USER 65534:65534
EXPOSE 8080

ENTRYPOINT ["/james"]
CMD ["-config", "/etc/james/james.yaml"]
