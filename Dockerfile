# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
# go.sum must only contain github.com/go-telegram/bot v1.27.0 (zero transitive deps).
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o adbot .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /src/adbot /adbot
EXPOSE 3000
ENTRYPOINT ["/adbot"]