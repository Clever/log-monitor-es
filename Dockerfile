FROM alpine:3.10
ENTRYPOINT [ "/bin/log-monitor-es" ]
RUN apk add ca-certificates && update-ca-certificates

COPY ./kvconfig.yml /bin/kvconfig.yml
COPY ./log-monitor-es /bin/log-monitor-es
