# bwrap-e2e 运行镜像：为 CI 沙箱门禁提供真实可用的 bubblewrap。
# ubuntu-latest 不自带 bubblewrap，沙箱边界用例会静默 skip；本镜像显式安装，
# 并由 CI 以 privileged 容器运行，避免宿主 userns 限制成为不确定变量。
# Author: fxbin
FROM golang:1.26

RUN apt-get update \
    && apt-get install -y --no-install-recommends bubblewrap \
    && rm -rf /var/lib/apt/lists/*
