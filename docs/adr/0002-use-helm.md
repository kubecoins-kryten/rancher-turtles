# 2. Use helm charts for releases

* Status: proposed
* Date: 2023-07-27
* Authors: @Danil-Grigorev
* Deciders: @richardcase @alexander-demicev @furkatgofurov7 @salasberryfin @Danil-Grigorev @mjura

## Context

As the operator needs to have a regular release process, we need to decide how we would structure our releases and what approved tooling to use. Current operator code release process comes from [cluster-api-operator](https://github.com/kubernetes-sigs/cluster-api-operator/). Due to different requirements on the projects, belonging to different ecosystems, usage of different CI systems, etc. we need to choose the way forward with structuring our code for release.

## Decision

For the operator releases we would use [helm charts](https://helm.sh/docs/topics/charts/). We will follow recommended practices, versioning and ensure compatibility with rancher packaging strategy and releases. We will follow the best practices and lean towards helm ecosystem in general.

This strategy aligns with rancher release process. 

## Consequences

- The project will have a recognizable chart structure, will use appropriate tooling and follow the versioning patterns.
- Each helm chart release will have a dedicated image, built to use in conjunction with installed helm charts.
- We, as a team will maintain a published copy of our helm chart.
