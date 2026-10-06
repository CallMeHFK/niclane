# Device binding (SO_BINDTODEVICE) needs CAP_NET_ADMIN on older kernels:
#   docker run --cap-add NET_ADMIN ...
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /niclane ./cmd/niclane

FROM gcr.io/distroless/static-debian12
COPY --from=build /niclane /niclane
ENTRYPOINT ["/niclane"]
