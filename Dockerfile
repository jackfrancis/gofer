# gofer web tier. The binary is built on the HOST (`make image` -> `build-linux`),
# not inside this stage, because the selected agent-runtime backend consumes the AEI
# modules through local `replace` directives to a sibling checkout
# (../agent-execution-interface) that lies outside any single-module Docker build
# context; a host build resolves them natively. This image only packages the prebuilt
# static binary.
#
# distroless/static: no shell, non-root by default, ships CA certs for the
# outbound HTTPS to the OAuth providers, GitHub, and the chat model.
FROM gcr.io/distroless/static:nonroot
COPY bin/linux/server /server
# The scheduled backup (deploy/k8s/backup) runs this second binary from the same
# image, so the snapshotter always ships with the web tier it backs up.
COPY bin/linux/snapshot /snapshot
EXPOSE 8080
# Numeric UID/GID (distroless nonroot) so a runAsNonRoot pod can verify non-root.
USER 65532:65532
ENTRYPOINT ["/server"]
