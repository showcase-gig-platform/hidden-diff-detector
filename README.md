# hidden-diff-detector

This product is in **EXPERIMENTAL** stage!

`hidden-diff-detector` detects diff not detected by `kubectl diff`.

## Description

In `kubectl diff`, some diffs cannot be detected.  
For examole, labels added using `kubectl edit`.

## How to use

Same usage as `kubectl diff`.

```
$ hidden-diff-detector -f manifest.yaml
```

```
$ cat manifest.yaml | hidden-diff-detector -f -
```

### flags

```
-e, --extra-config string   Path to extra config file.
-f, --filename string       Filename, directory, or URL to files contains the configuration to diff.
-h, --help                  help for use
-k, --kubeconfig string     Path to kubeconfig file. (default "~/.kube/config")
```

## Extra configuration
!!!this feature is in alpha!!!

```yaml:examlpe.yaml
ignoreResources:
  - configmap
fieldFilter: |
  metadata:
    labels:
      app.kubernetes.io/instance: {}
```

### ignoreResources

List of resources to exclude from evaluation.

### fieldFilter

YAML definition of the fields to exclude from evaluation.  
More example below

```yaml:more-examlpe.yaml
metadata:
  labels:
    labelKey1: {}
    labelKey2:
  annotations: {}
spec:
  template:
    spec:
      containers:
        - name: foo
        - env:
            - name: key1
```

This config means...

- `metadata.labels.labelKey1` is excluded
- `metadata.labels.labelKey2` is excluded
  - `{}` and empty value mean the same thing
- `metadata.annotations` is excluded
- `name=foo` object in `spec.template.spec.containers[]` is excluded
- `name=key1` object in `spec.template.spec.containers[].env[]` is excluded
  - exclude from all containers

## Known issue

### Very slow
If you enter a lot of manifests at once, it may take some time to get results.  
This is due to the limitation of kubernetes api (and my poor implementation).

### Non-existent resources
Not supported for non-existent resources.  
If you enter a resource that does not exist in the cluster, `kubectl diff` outputs diffs that will be added, but `hidden-diff-detector` outputs only error log that resource is not found.  
This is due to the internal use of the resource-replace mechanism.

### Unexpectedly noisy
Not a few elements added to the resource by kubernetes and controllers, and many more diffs were detected than expected.  
So we implemented a fieldFilter...
