FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends build-essential cmake git ca-certificates libcurl4-openssl-dev libssl-dev zlib1g-dev ninja-build && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch 1.11.935 --recurse-submodules --shallow-submodules https://github.com/aws/aws-sdk-cpp /src \
 && cmake -S /src -B /build -G Ninja -DCMAKE_BUILD_TYPE=Release -DBUILD_ONLY="s3;transfer" -DENABLE_TESTING=OFF -DAUTORUN_UNIT_TESTS=OFF -DBUILD_SHARED_LIBS=OFF -DCMAKE_INSTALL_PREFIX=/opt/aws \
 && cmake --build /build -j"$(nproc)" && cmake --install /build && rm -rf /build /src
