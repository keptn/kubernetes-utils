module github.com/keptn/kubernetes-utils

go 1.13

require (
	github.com/Azure/go-autorest/autorest v0.9.0
	github.com/keptn/go-utils v0.6.3-0.20201021140127-a974f80c5982
	helm.sh/helm/v3 v3.1.2
	k8s.io/api v0.17.2
	k8s.io/apimachinery v0.17.2
	k8s.io/client-go v0.17.2
	sigs.k8s.io/yaml v1.1.0
)

// Transitive requirement from Helm: See https://github.com/helm/helm/blob/v3.1.2/go.mod
replace (
	github.com/Azure/go-autorest => github.com/Azure/go-autorest v13.3.2+incompatible
	github.com/docker/distribution => github.com/docker/distribution v0.0.0-20191216044856-a8371794149d
)
