# gRPC vs Rest vs GraphQL
https://www.youtube.com/watch?v=uH0SxYdsjv4: compares rest vs graphql vs grpc. a bunch of apps on k8
- grpc takes a minute to start up and is actually pretty bad latency until you scale to above 10k requests/second
- summary
    - graphql is the worst at everything
    - grpc is best at pretty much everything but only at scale. it dramatically outclasses rest despite having 80k requests vs 50k requests. at that level cpu useage is similar, but it's just way less for all other resources and much less latency
    - the last key point is that rest can only handle like 60k requests per second, gprc can handle 90k

    - latency (p90): grpc can handle enterpise levels of traffic much better than graphql or rest. it's worse at first, but passes graphql at like 10k requests/sec, and passes rest at like 50k requests/sec. once we get this much traffic the difference is stark
    - cpu: gpt is consistantly ~12% higher than rest. graphql sucks
    - client network usage: grpc is much lower (almost half)
    - app memory usage: grpc is way, way, way lower. think 7.5mb vs 55mb for rest at 55k requests/sec

    **conclusion:**
        - rest is fine for most things. like web apps
        - grpc is much better for microservice, anything high througput, and can improve user experience


# redis vs valkey
- main difference is cpu usage. redis is the same for basic loads, but at scale is 37% vs 65%
- handle about the same requests/second
- memory: redis is worse at scale but not a huge difference
- network: same
    **conclusion**
        - redis just for the cpu power saving