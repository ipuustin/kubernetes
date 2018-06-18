package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_binary",
)

go_binary(
    name = "cpu-policy-static",
    srcs = ["cpu-policy-static.go"],
    deps = [
        "//pkg/kubelet/apis/cpuplugin/v1alpha:go_default_library",
        "//pkg/kubelet/cm/cpumanager/stub:go_default_library",
        "//pkg/kubelet/cm/cpumanager/topology:go_default_library",
        "//vendor/k8s.io/api/core/v1:go_default_library",
        "//pkg/apis/core/v1/helper/qos:go_default_library",
    ],
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [":package-srcs"],
    tags = ["automanaged"],
)
