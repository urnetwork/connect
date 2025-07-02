FROM alpine:3.18
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/
ARG TARGETPLATFORM
ARG warp_env
ENV WARP_ENV=${warp_env}

RUN apk add --no-cache \
      ca-certificates \
 && update-ca-certificates

# Copy your cross-compiled provider binary, ensure it's executable
COPY build/${TARGETPLATFORM}/provider /usr/local/sbin/bringyour-provider
RUN chmod +x /usr/local/sbin/bringyour-provider

EXPOSE 80
STOPSIGNAL SIGTERM
# additional arguments can be appended to the end
ENTRYPOINT ["/usr/local/sbin/bringyour-provider", "auth-provide", "-p", "80"]
