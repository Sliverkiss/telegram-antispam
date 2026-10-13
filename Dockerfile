FROM golang:1.23-alpine AS build
WORKDIR /src
COPY app/go.mod ./
COPY app/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o adbot .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /src/adbot /adbot
EXPOSE 3000
ENTRYPOINT ["/adbot"]