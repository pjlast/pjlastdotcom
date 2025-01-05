# Build.
FROM golang:1.23 AS build-stage
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . /app
RUN CGO_ENABLED=0 GOOS=linux go build -o /entrypoint

# Deploy.
FROM debian:latest AS release-stage

RUN apt-get update && apt-get install -y --no-install-recommends openssh-client

WORKDIR /
COPY --from=build-stage /entrypoint /entrypoint
COPY --from=build-stage /app/css /css
COPY --from=build-stage /app/js /js
COPY --from=build-stage /app/images /images

EXPOSE 8080
EXPOSE 23423

ENTRYPOINT ["/entrypoint"]
