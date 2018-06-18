load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "cpu-plugin.go",
        "conversion.go",
        "state.go"
        "log.go",
    ],
    importpath = "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/stub",
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/kubelet/apis/deviceplugin/v1beta1:go_default_library",        
        "//vendor/google.golang.org/grpc:go_default_library",
        "//vendor/k8s.io/api/core/v1:go_default_library",
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
    visibility = ["//visibility:public"],
)
