FROM gliderlabs/alpine:3.3
ENTRYPOINT [ "/bin/log-monitor-es" ]
RUN apk-install ca-certificates
COPY ./log-monitor-es /bin/log-monitor-es
