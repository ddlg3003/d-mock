# The binary is built on the host by stack.load.yaml (build.pre), so this needs no base image.
FROM scratch
COPY examples/checkout/service/checkout-linux /checkout
ENTRYPOINT ["/checkout"]
