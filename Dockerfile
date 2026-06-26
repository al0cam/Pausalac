# Build a static binary (pure-Go sqlite -> no CGO), ship it on scratch.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /pausalac .

FROM scratch
COPY --from=build /pausalac /pausalac
ENV DB_PATH=/data/pausalac.db PORT=8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/pausalac"]
