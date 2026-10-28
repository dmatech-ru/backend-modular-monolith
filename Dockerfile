FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
# imports.go не хранится в git: по умолчанию в образ входят все модули.
RUN [ -f cmd/bootstrap/imports.go ] || go run ./cmd/builder -all
RUN CGO_ENABLED=0 go build -mod=vendor -tags dynamic -trimpath -ldflags="-s -w" -o /out/app ./cmd/bootstrap

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY configs /configs
ENV APP_ENV=production \
    GATEWAY_CONFIG_PATH=/configs/gateway.yaml \
    IDENTITY_CONFIG_PATH=/configs/identity.yaml \
    CAPTCHA_CONFIG_PATH=/configs/captcha.yaml \
    NOTIFICATIONS_CONFIG_PATH=/configs/notifications.yaml
EXPOSE 1987
ENTRYPOINT ["/app"]
