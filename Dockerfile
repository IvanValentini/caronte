# Build backend with go
FROM golang:1.16 AS BACKEND_BUILDER

WORKDIR /

# Install tools and libraries
RUN apt-get update && \
	DEBIAN_FRONTEND=noninteractive apt-get install -qq \
	git \
	pkg-config \
	libpcap-dev \
	$(if [ "$(dpkg --print-architecture)" = "amd64" ]; then echo "libhyperscan-dev"; fi)

# Perform git pull for arm64 architecture and add vectorscan to path
RUN if [ "$(dpkg --print-architecture)" = "arm64" ]; then \
	DEBIAN_FRONTEND=noninteractive apt-get install -qq git g++ cmake libboost-all-dev ragel && \
	git clone https://github.com/VectorCamp/vectorscan.git && \
	cd vectorscan && \
	git checkout feature/add-arm-support && \
	cmake -G"Unix Makefiles" && \
	make -j8 . && \
	make install; \
	fi

WORKDIR /caronte

COPY . ./

RUN export VERSION=$(git describe --tags --abbrev=0) && \
    go mod download && \
    go build -ldflags "-X main.Version=$VERSION" && \
	mkdir -p build && \
	cp -r caronte pcaps/ scripts/ shared/ test_data/ build/


# Build frontend via yarn
FROM node:16 as FRONTEND_BUILDER

WORKDIR /caronte-frontend

COPY ./frontend ./

RUN yarn install --network-timeout 600000 && yarn build --production=true


# LAST STAGE
FROM ubuntu:20.04

COPY --from=BACKEND_BUILDER /caronte/build /opt/caronte

COPY --from=FRONTEND_BUILDER /caronte-frontend/build /opt/caronte/frontend/build

RUN apt-get update && \
	DEBIAN_FRONTEND=noninteractive apt-get install -qq \
	libpcap-dev \
	libhyperscan-dev && \
	rm -rf /var/lib/apt/lists/*

ENV GIN_MODE release

ENV MONGO_HOST mongo

ENV MONGO_PORT 27017

WORKDIR /opt/caronte

ENTRYPOINT ./caronte -mongo-host ${MONGO_HOST} -mongo-port ${MONGO_PORT} -assembly_memuse_log
