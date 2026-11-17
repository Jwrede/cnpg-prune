FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /cnpg-prune ./cmd/cnpg-prune

FROM gcr.io/distroless/static:nonroot
COPY --from=build /cnpg-prune /cnpg-prune
USER 65532:65532
ENTRYPOINT ["/cnpg-prune"]
