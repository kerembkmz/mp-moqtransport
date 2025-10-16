# --- Build stage ---
FROM golang:1.23-alpine AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY . .
# resolve deps using the repo root so "replace ../../" works
RUN go mod download
# build the example binary
WORKDIR /src/examples/multipath
RUN go build -o /out/multipath-moq .

# --- Runtime stage ---
FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/multipath-moq /usr/local/bin/multipath-moq
# include certs if your server reads localhost.pem by default
COPY --from=build /src/examples/multipath/localhost.pem /app/
COPY --from=build /src/examples/multipath/localhost-key.pem /app/
RUN apk add --no-cache iproute2
ENTRYPOINT ["/usr/local/bin/multipath-moq"]