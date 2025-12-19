#!/bin/bash

# Insight Trace Deploy Script
# KETI APOLLO System

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DEPLOY_DIR="$PROJECT_DIR/deployments"

ACTION="${1:-apply}"
NAMESPACE="${2:-keti}"

echo "=============================================="
echo "  Insight Trace - Deploy Script"
echo "  KETI APOLLO System Component"
echo "=============================================="

cd "$PROJECT_DIR"

case "$ACTION" in
    apply|a)
        echo ""
        echo "Deploying Insight Trace components..."

        # Create namespace if not exists
        kubectl create namespace $NAMESPACE 2>/dev/null || true

        # Apply ConfigMap
        echo "Applying ConfigMap..."
        kubectl apply -f "$DEPLOY_DIR/configmap.yaml" -n $NAMESPACE

        echo ""
        echo "=============================================="
        echo "  Deployment Complete!"
        echo "=============================================="
        echo ""
        echo "Insight Trace ConfigMap deployed to namespace: $NAMESPACE"
        echo ""
        echo "To add Insight Trace sidecar to your pods:"
        echo "  1. Add 'shareProcessNamespace: true' to Pod spec"
        echo "  2. Add the sidecar container from sidecar-template.yaml"
        echo "  3. Set CONTAINER_NAME env var to your main container name"
        echo ""
        echo "Example pod with sidecar:"
        echo "  kubectl apply -f $DEPLOY_DIR/example-pod-with-sidecar.yaml"
        ;;

    delete|d)
        echo ""
        echo "Deleting Insight Trace components..."

        kubectl delete -f "$DEPLOY_DIR/configmap.yaml" -n $NAMESPACE --ignore-not-found

        echo ""
        echo "Insight Trace ConfigMap deleted"
        ;;

    example)
        echo ""
        echo "Deploying example pod with Insight Trace sidecar..."

        # Apply ConfigMap first
        kubectl apply -f "$DEPLOY_DIR/configmap.yaml" -n $NAMESPACE 2>/dev/null || true

        # Apply example pod
        kubectl apply -f "$DEPLOY_DIR/example-pod-with-sidecar.yaml"

        echo ""
        echo "Example pod deployed: pytorch-training-with-insight"
        echo ""
        echo "Check status:"
        echo "  kubectl get pod pytorch-training-with-insight"
        echo ""
        echo "View Insight Trace metrics:"
        echo "  kubectl port-forward pod/pytorch-training-with-insight 9090:9090"
        echo "  curl http://localhost:9090/metrics"
        echo "  curl http://localhost:9090/signature"
        ;;

    *)
        echo "Usage: $0 {apply|delete|example} [namespace]"
        echo ""
        echo "Commands:"
        echo "  apply, a   - Deploy ConfigMap"
        echo "  delete, d  - Delete ConfigMap"
        echo "  example    - Deploy example pod with sidecar"
        echo ""
        echo "Default namespace: keti"
        exit 1
        ;;
esac
