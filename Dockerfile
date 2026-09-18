FROM node:22-bookworm-slim AS frontend
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend ./
RUN npm run build

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=frontend /src/cmd/local-picsum/static ./cmd/local-picsum/static
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/local-picsum ./cmd/local-picsum

FROM debian:bookworm-slim
RUN useradd --system --uid 10001 app
COPY --from=build /out/local-picsum /usr/local/bin/local-picsum
USER app
ENV PORT=8080 DATA_DIR=/data LIBRARY_ROOT=/photos REFRESH_INTERVAL=6h
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/local-picsum"]
