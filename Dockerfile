ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/k3s-dashboard ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/k3s-dashboard /app/k3s-dashboard

EXPOSE 8080

ENTRYPOINT ["/app/k3s-dashboard"]