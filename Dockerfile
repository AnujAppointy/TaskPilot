FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/taskpilot ./cmd/taskpilot

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/taskpilot /app/taskpilot
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/taskpilot"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--db", "/data/taskpilot.db", "--production"]
