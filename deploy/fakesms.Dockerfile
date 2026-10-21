FROM golang:1.26-alpine AS build
ENV GOTOOLCHAIN=auto CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/fakesms ./cmd/fakesms

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/fakesms /fakesms
EXPOSE 8091
ENTRYPOINT ["/fakesms"]
