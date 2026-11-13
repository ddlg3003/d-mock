# The binary is built on the host by containers/dmock.yaml (build.pre), so this needs no base image.
FROM scratch
COPY examples/orders/toy/orders-linux /orders
ENTRYPOINT ["/orders"]
