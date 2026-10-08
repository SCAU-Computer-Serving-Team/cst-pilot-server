FROM caddy:2-alpine@sha256:6aeddd44c3078b0f9a35206472a11420648a79c184603ef95957d0a20044cb2b
RUN setcap -r /usr/bin/caddy
COPY --chmod=0755 telemetry-receiver /usr/local/bin/telemetry-receiver
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/telemetry-receiver"]
CMD ["serve"]
