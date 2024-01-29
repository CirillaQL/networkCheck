FROM public.ecr.aws/docker/library/golang:1.21 AS build

COPY . /src
RUN cd /src && go mod tidy
RUN go env -w CGO_ENABLED=0
RUN cd /src && go build -o checkNetwork

FROM public.ecr.aws/docker/library/debian:bullseye AS publish
WORKDIR /app
COPY --from=build src/checkNetwork /app/checkNetwork

ENV TZ=Asia/Tokyo
RUN echo $TZ > /etc/timezon
RUN apt-get update && apt-get install -y ca-certificates openssl
RUN chmod +x /app/checkNetwork

ENTRYPOINT ["/app/checkNetwork"]