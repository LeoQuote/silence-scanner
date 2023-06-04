FROM golang:1.20 AS builder

COPY . /app
WORKDIR /app

# Toggle CGO based on your app requirement. CGO_ENABLED=1 for enabling CGO
RUN CGO_ENABLED=1 go build -ldflags '-s -w -extldflags "-static"' -o /app/silence-scanner .
# Use below if using vendor
# RUN CGO_ENABLED=0 go build -mod=vendor -ldflags '-s -w -extldflags "-static"' -o /app/appbin *.go

FROM debian:stable-slim

# Following commands are for installing CA certs (for proper functioning of HTTPS and other TLS)
RUN apt-get update && apt-get install -y --no-install-recommends \
		ca-certificates  \
        netbase \
        && rm -rf /var/lib/apt/lists/ \
        && apt-get autoremove -y && apt-get autoclean -y

# Add new user 'appuser'. App should be run without root privileges as a security measure

COPY --from=builder /app /usr/bin/


ENTRYPOINT ["silence-scanner"]