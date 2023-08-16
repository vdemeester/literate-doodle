FROM quay.io/buildah/stable

RUN dnf update -y && dnf install -y git htop

USER 0

CMD id && htop