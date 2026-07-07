FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/sss-node ./cmd/sss-node

FROM alpine:3.22
# poppler-utils provides pdftotext for optional PDF content search
RUN apk add --no-cache ca-certificates tzdata poppler-utils
COPY --from=build /out/sss-node /app/sss-node
ENV SSS_CONFIG=/config/config.yaml
VOLUME ["/config", "/data"]
EXPOSE 37373
ENTRYPOINT ["/app/sss-node"]
CMD ["run"]
