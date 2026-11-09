FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o zcode2api ./cmd/zcode2api

FROM node:22-alpine
WORKDIR /app
COPY --from=build /app/zcode2api .
RUN mkdir -p data
EXPOSE 3101
CMD ["./zcode2api"]
