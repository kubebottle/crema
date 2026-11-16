<p align="center">
  <img src="assets/crema-mascot.png" alt="crema mascot" width="220" />
</p>

# crema

> *Crema is the thin golden layer on top of an espresso — just like the thin writable filesystem layer sitting on top of a heavy GPU base image.*

Declarative GPU Workspace management for Kubernetes.

## Why

In GPU development environments, users routinely install Python packages, tweak dotfiles, and keep local tools in the container's root filesystem. On standard Kubernetes, changing GPU counts (e.g. `0 → 1 → 4` GPUs), pausing to release idle GPUs, or rescheduling a Pod wipes that writable layer clean.

**crema** bridges the gap between stateful dev machines and Kubernetes Pods:

- **Stateful GPU Scaling**: Scale GPUs up, down, or detach to zero GPUs without losing installed system packages, dotfiles, or environment setup.
- **Pause & Resume**: Release expensive GPU resources when idle and resume your exact workspace later.
- **Versioned Rollbacks**: Track workspace environment snapshots and roll back to previous states when needed.
- **Kubernetes Native**: Manage workspaces declaratively without sacrificing standard scheduling, GPU device plugins, or multi-node portability.

Inspired by [gpu-docker-api](https://github.com/mayooot/gpu-docker-api), redesigned for Kubernetes.

## Architecture

> See [docs/design.md](docs/design.md) for the full architecture, CRD specification, and state machine details.

```mermaid
flowchart TB
    User(["User / kubectl"]) -->|"Apply GPUWorkspace"| API["Kubernetes API Server"]

    subgraph CP["Control Plane · crema-controller"]
        direction LR
        Reconciler["Reconciler & State Machine"] --> PodOrch["Pod / PVC Orchestrator\n(Recreate + Soft Affinity)"]
        Reconciler --> GC["Revision History & GC"]
    end

    API <-->|"Watch / Status"| Reconciler
    PodOrch -->|"Create / Delete Pod & PVC"| API
    GC -->|"Delete expired manifests"| Registry[("OCI Registry\nHarbor / registry:2")]

    subgraph Node["Worker Node · DaemonSet & Runtime"]
        direction TB
        Agent["crema-agent (gRPC)\nPause -> Diff -> zstd -> OCI Push"]
        Containerd["containerd (k8s.io)\nOverlayFS lowerdir + upperdir"]
        Pod["Workspace Pod\nRootfs + PVC (/workspace)"]

        Agent -->|"Task.Pause / diff.Compare / Resume"| Containerd
        Containerd --> Pod
    end

    Reconciler -->|"gRPC CommitContainer"| Agent
    Agent -->|"HEAD base (skip) + PUT delta layer"| Registry
    Containerd -->|"Pull committed image (delta only)"| Registry

    classDef control fill:#eef4ff,stroke:#4a7bd0,stroke-width:1.5px,color:#1e293b
    classDef agent fill:#fffbeb,stroke:#d97706,stroke-width:1.5px,color:#451a03
    classDef infra fill:#f8fafc,stroke:#64748b,stroke-width:1.5px,color:#0f172a
    classDef registry fill:#f0fdf4,stroke:#16a34a,stroke-width:1.5px,color:#14532d

    class Reconciler,PodOrch,GC control
    class Agent agent
    class API,Containerd,Pod infra
    class Registry registry
```

## Status

🚧 **Work in progress** — currently in early design and prototyping.

## License

[Apache-2.0](LICENSE)
