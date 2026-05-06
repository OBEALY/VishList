FROM node:24-alpine AS web-build
WORKDIR /web
COPY frontend/package.json ./
RUN npm install
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS api-build
WORKDIR /src
COPY backend/ ./
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/vislist ./cmd/server

FROM alpine:3.22
WORKDIR /app
RUN adduser -D -H vislist
COPY --from=api-build /out/vislist /app/vislist
COPY --from=web-build /web/dist /app/web
RUN mkdir -p /app/uploads && chown -R vislist:vislist /app
USER vislist
EXPOSE 8080
CMD ["/app/vislist"]
