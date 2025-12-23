#!/bin/bash
# Insight Trace (Sidecar) - 로그 조회 스크립트

CONTAINER_NAME="insight-trace"

# insight-trace 사이드카가 있는 Pod 찾기
PODS=$(kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null | while read line; do
    NS=$(echo $line | cut -d'/' -f1)
    POD=$(echo $line | cut -d'/' -f2)
    if kubectl get pod -n $NS $POD -o jsonpath='{.spec.containers[*].name}' 2>/dev/null | grep -q "$CONTAINER_NAME"; then
        echo "$NS/$POD"
    fi
done)

if [ -z "$PODS" ]; then
    echo "Error: No pods with insight-trace sidecar found"
    exit 1
fi

echo "=== Insight Trace 사이드카가 있는 Pod 목록 ==="
echo "$PODS"
echo "=============================================="
echo ""

# 첫 번째 Pod 선택
FIRST_POD=$(echo "$PODS" | head -1)
NS=$(echo $FIRST_POD | cut -d'/' -f1)
POD=$(echo $FIRST_POD | cut -d'/' -f2)

echo "로그 조회: $NS/$POD (container: $CONTAINER_NAME)"
echo ""

# 옵션 처리
if [ "$1" == "-f" ] || [ "$1" == "--follow" ]; then
    kubectl logs -n $NS $POD -c $CONTAINER_NAME -f
elif [ "$1" == "--tail" ]; then
    LINES=${2:-100}
    kubectl logs -n $NS $POD -c $CONTAINER_NAME --tail=$LINES
elif [ "$1" == "--pod" ]; then
    # 특정 Pod 지정
    kubectl logs -n $(echo $2 | cut -d'/' -f1) $(echo $2 | cut -d'/' -f2) -c $CONTAINER_NAME --tail=100
else
    kubectl logs -n $NS $POD -c $CONTAINER_NAME --tail=100
fi
