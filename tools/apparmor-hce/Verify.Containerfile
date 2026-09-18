# syntax=docker/dockerfile:1
ARG HCE_BUILDER_IMAGE
FROM ${HCE_BUILDER_IMAGE} AS audit
ARG HCE_BUILDER_IMAGE
ARG TARGETARCH
ENV PYTHONDONTWRITEBYTECODE=1 LC_ALL=C
RUN set -eu; . /etc/os-release; test "${ID:-}" = hce && test "${VERSION_ID:-}" = 2.0
RUN dnf install -y --setopt=install_weak_deps=False python3 rpm binutils gnupg2 \
    && dnf clean all
# Execute the current audit implementation, never scripts from the artifact.
COPY lib /audit/lib/
COPY artifacts /out/
RUN --mount=type=secret,id=apparmor_trusted_keyring,required=true \
    python3 /audit/lib/hce_stage.py repeat --arch "$TARGETARCH" --builder-image "$HCE_BUILDER_IMAGE"
FROM scratch AS export
COPY --from=audit /out/ /
