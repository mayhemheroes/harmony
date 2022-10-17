FROM ubuntu:20.04 as builder

RUN ln -snf /usr/share/zoneinfo/$CONTAINER_TIMEZONE /etc/localtime && echo $CONTAINER_TIMEZONE > /etc/timezone

RUN DEBIAN_FRONTEND=noninteractive \
	apt-get update && apt-get install -y build-essential tzdata pkg-config \
	wget clang git

RUN wget https://go.dev/dl/go1.19.1.linux-amd64.tar.gz
RUN rm -rf /usr/local/go && tar -C /usr/local -xzf go1.19.1.linux-amd64.tar.gz
ENV PATH=$PATH:/usr/local/go/bin

#RUN export GO111MODULE=off
#ENV GO111MODULE=off

ADD . /harmony
WORKDIR /harmony

# package myfuzz
ADD fuzzers/fuzz_numeric_newdecfromstr.go ./fuzzers/
WORKDIR ./fuzzers/
RUN go install github.com/dvyukov/go-fuzz/go-fuzz@latest github.com/dvyukov/go-fuzz/go-fuzz-build@latest
#ENV GO111MODULE=off
RUN go get github.com/harmony-one/harmony
RUN /root/go/bin/go-fuzz-build -libfuzzer -o fuzznewdecfrmstr.a
RUN clang -fsanitize=fuzzer fuzznewdecfrmstr.a -o fuzz_newdecfromstr

#FROM fuzzers/go-fuzz:1.2.0
FROM ubuntu:20.04
COPY --from=builder /harmony/fuzzers/fuzz_newdecfromstr  /

ENTRYPOINT []
CMD ["/fuzz_newdecfromstr"]
