# sofmat — llama.cpp ggml-rpc-server (CUDA) for a stage worker.
# LM Studio ships only llama-server; the PP-RPC baseline needs rpc-server.
# The official release has no prebuilt Linux x64 CUDA -> we build it here.
# Runs on any worker with an NVIDIA GPU: `docker run --gpus all --network host`.
FROM nvidia/cuda:12.8.0-devel-ubuntu22.04

# Compute capability of your GPU. Override at build time:
#   --build-arg CUDA_ARCH=xx   (e.g. 86, 89, 90, 120)
ARG CUDA_ARCH=120

RUN apt-get update && apt-get install -y --no-install-recommends \
        git cmake build-essential libcurl4-openssl-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 https://github.com/ggml-org/llama.cpp /llama.cpp
WORKDIR /llama.cpp

# GGML_RPC=ON produces the ggml-rpc-server binary (target under tools/rpc/,
# renamed from 'rpc-server' -> 'ggml-rpc-server').
# libggml-cuda uses the CUDA Driver API and ends up with DT_NEEDED libcuda.so.1
# (the real driver soname). The toolkit ships only the stub libcuda.so -> we
# create the .so.1 link and give rpath-link to the stubs to resolve the
# transitive dependency when linking the EXECUTABLE. At runtime the real driver
# provides it (--gpus).
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
