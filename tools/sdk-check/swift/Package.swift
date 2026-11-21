// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "check",
    platforms: [.macOS(.v12)],
    dependencies: [
        .package(url: "https://github.com/awslabs/aws-sdk-swift", exact: "1.8.33"),
    ],
    targets: [
        .executableTarget(
            name: "check",
            dependencies: [
                .product(name: "AWSS3", package: "aws-sdk-swift"),
            ],
            path: "Sources/check"
        ),
    ]
)
