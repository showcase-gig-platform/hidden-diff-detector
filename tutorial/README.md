# Tutorial
Let's try how it works.

## prepare

- kubernetes cluster
- kubectl
- hidden-diff-detector

## create configmap

```shell
tutorial $ kubectl apply -f configmap.yaml
> configmap/sample created
tutorial $ kubectl get cm sample
> NAME     DATA   AGE
> sample   1      3s
```

## edit configmap

```shell
tutorial $ kubectl edit cm sample
```
Add some labels and data.  
This time, we added label `key2: value2`, data `data2: bar` and, annotation `key1: value1`.

And, check configmap.

```shell
tutorial $ kubectl get cm sample -o yaml
> apiVersion: v1
> data:
>   data1: foo
>   data2: bar  # added
> kind: ConfigMap
> metadata:
>   annotations:    # added
>     key1: value1  # added
>   creationTimestamp: "2022-12-02T07:16:41Z"
>   labels:
>     key1: value1
>     key2: value2  # added
>   name: sample
>   namespace: default
>   resourceVersion: "6096208"
>   uid: 52d11491-2c4a-4b9b-91a0-0039de4f3ebe
```

## kubectl diff
First, run `kubectl diff`.

```shell
tutorial $ kubectl diff -f configmap.yaml
>
tutorial $
```
Elements added with `kubectl edit` are not detected as diff.

## hidden-diff-detector
Next, run `hidden-diff-detector`

```shell
tutorial $ hidden-diff-detector -f configmap.yaml 
> diff -u -N .tmp/LIVE-4284887158/v1.ConfigMap.default.sample .tmp/REPLACED-2601923827/v1.ConfigMap.default.sample
> --- .tmp/LIVE-4284887158/v1.ConfigMap.default.sample    2022-12-02 16:27:30.000000000 +0900
> +++ .tmp/REPLACED-2601923827/v1.ConfigMap.default.sample        2022-12-02 16:27:30.000000000 +0900
> @@ -1,15 +1,11 @@
>  apiVersion: v1
>  data:
>    data1: foo
> -  data2: bar
>  kind: ConfigMap
>  metadata:
> -  annotations:
> -    key1: value1
>    creationTimestamp: "2022-12-02T07:16:41Z"
>    labels:
>      key1: value1
> -    key2: value2
>    name: sample
>    namespace: default
>    resourceVersion: "6096208"
> 
```

Detected diffs added by kubectl edit 😎

## cleaning

```shell
tutorial $ kubectl delete -f configmap.yaml
```

Thank you!
