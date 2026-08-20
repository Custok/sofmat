# sofmat — llama.cpp ggml-rpc-server with CUDA, for a GPU worker node.
# Many llama.cpp distributions ship only llama-server; the PP-RPC baseline
# needs ggml-rpc-server, and there is no official prebuilt Linux x64 CUDA
# binary -> build it here. Runs on any NVIDIA GPU: `docker run --gpus all`.
FROM nvidia/cuda:12.8.0-devel-ubuntu22.04

# Set to your GPU's CUDA compute capability for a smaller, faster single-arch
# image (e.g. --build-arg CUDA_ARCH=90 / 89 / 120). Default builds all major
# archs (portable, larger). See CMAKE_CUDA_ARCHITECTURES.
ARG CUDA_ARCH=all-major

RUN apt-get update && apt-get install -y --no-install-recommends \
        git cmake build-essential libcurl4-openssl-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 https://github.com/ggml-org/llama.cpp /llama.cpp
WORKDIR /llama.cpp

# GGML_RPC=ON produces the ggml-rpc-server target (tools/rpc/; renamed from the
# older 'rpc-server'). libggml-cuda uses the CUDA Driver API and ends up with a
# DT_NEEDED on libcuda.so.1 (the real driver soname). The -devel image ships
# only the stub libcuda.so -> create the .so.1 link and give the linker an
# rpath-link to the stubs so the EXECUTABLE link resolves the transitive
# dependency. At runtime the real driver (via --gpus) provides the symbols.
RUN ln -sf /usr/local/cuda/lib64/stubs/libcuda.so /usr/local/cuda/lib64/stubs/libcuda.so.1
ENV LIBRARY_PATH=/usr/local/cuda/lib64/stubs
RUN cmake -B build \
        -DGGML_CUDA=ON \
        -DGGML_RPC=ON \
        -DCMAKE_CUDA_ARCHITECTURES=${CUDA_ARCH} \
        -DCMAKE_BUILD_TYPE=Release \
        -DLLAMA_CURL=OFF \
        -DCMAKE_SHARED_LINKER_FLAGS="-L/usr/local/cuda/lib64/stubs" \
        -DCMAKE_EXE_LINKER_FLAGS="-L/usr/local/cuda/lib64/stubs -Wl,-rpath-link,/usr/local/cuda/lib64/stubs" \
    && cmake --build build --config Release -j --target ggml-rpc-server

# ggml-rpc-server: exposes this host's GPU to the llama.cpp main via --rpc host:port.
EXPOSE 50052
ENTRYPOINT ["/llama.cpp/build/bin/ggml-rpc-server"]
CMD ["--host", "0.0.0.0", "--port", "50052"]
