#!/bin/bash
# ============================================
# Argo Workflow + Insight Trace 연동 테스트 스크립트
# KETI AI Storage System
# ============================================

set -e

# 색상 정의
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOYMENTS_DIR="$SCRIPT_DIR/../deployments"

print_header() {
    echo ""
    echo -e "${BLUE}=== $1 ===${NC}"
    echo ""
}

print_success() {
    echo -e "${GREEN}✓ $1${NC}"
}

print_warning() {
    echo -e "${YELLOW}⚠ $1${NC}"
}

print_error() {
    echo -e "${RED}✗ $1${NC}"
}

# 1. Argo Workflows 설치 확인
check_argo() {
    print_header "1. Argo Workflows 설치 확인"

    if kubectl get crd workflows.argoproj.io &>/dev/null; then
        print_success "Argo Workflows CRD 존재"
    else
        print_error "Argo Workflows가 설치되지 않았습니다."
        echo ""
        echo "설치 방법:"
        echo "  kubectl create namespace argo"
        echo "  kubectl apply -n argo -f https://github.com/argoproj/argo-workflows/releases/download/v3.5.0/install.yaml"
        exit 1
    fi

    # Argo 컨트롤러 확인
    if kubectl get pods -n argo -l app=workflow-controller 2>/dev/null | grep -q Running; then
        print_success "Argo Workflow Controller 실행 중"
    elif kubectl get pods -n argocd -l app.kubernetes.io/name=argocd-application-controller 2>/dev/null | grep -q Running; then
        print_warning "Argo CD는 있지만, Argo Workflows가 필요합니다."
    fi
}

# 2. Insight Trace 이미지 확인
check_insight_trace() {
    print_header "2. Insight Trace 이미지 확인"

    # Docker 이미지 확인
    if docker images | grep -q "ketidevit2/insight-trace"; then
        print_success "insight-trace 이미지 존재 (Docker)"
    else
        print_warning "insight-trace 이미지가 없습니다. 빌드합니다..."

        cd "$SCRIPT_DIR/.."
        if [ -f "scripts/1.build-image.sh" ]; then
            ./scripts/1.build-image.sh
        else
            # 수동 빌드
            CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/insight-trace cmd/main.go
            docker build -t ketidevit2/insight-trace:latest .
        fi
    fi
}

# 3. 테스트 워크플로우 배포
deploy_test_workflow() {
    print_header "3. 테스트 워크플로우 배포"

    WORKFLOW_FILE="$DEPLOYMENTS_DIR/test-argo-workflow.yaml"

    if [ ! -f "$WORKFLOW_FILE" ]; then
        print_error "테스트 워크플로우 파일이 없습니다: $WORKFLOW_FILE"
        exit 1
    fi

    echo "워크플로우 생성 중..."
    WORKFLOW_NAME=$(kubectl create -f "$WORKFLOW_FILE" -o jsonpath='{.metadata.name}')
    print_success "워크플로우 생성됨: $WORKFLOW_NAME"

    echo "$WORKFLOW_NAME" > /tmp/test-workflow-name
}

# 4. 워크플로우 상태 확인
check_workflow_status() {
    print_header "4. 워크플로우 상태 확인"

    if [ ! -f /tmp/test-workflow-name ]; then
        print_error "워크플로우 이름을 찾을 수 없습니다."
        return 1
    fi

    WORKFLOW_NAME=$(cat /tmp/test-workflow-name)

    echo "워크플로우: $WORKFLOW_NAME"
    echo ""

    # 상태 확인 (최대 2분 대기)
    for i in {1..24}; do
        PHASE=$(kubectl get workflow "$WORKFLOW_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")

        case "$PHASE" in
            "Succeeded")
                print_success "워크플로우 완료!"
                break
                ;;
            "Failed")
                print_error "워크플로우 실패"
                kubectl get workflow "$WORKFLOW_NAME" -o yaml | grep -A 5 "message:"
                break
                ;;
            "Running")
                echo -ne "\r실행 중... ($i/24) [Phase: $PHASE]    "
                ;;
            *)
                echo -ne "\r대기 중... ($i/24) [Phase: $PHASE]    "
                ;;
        esac

        sleep 5
    done
    echo ""
}

# 5. Insight Trace 로그 확인
check_insight_trace_logs() {
    print_header "5. Insight Trace 로그 확인"

    if [ ! -f /tmp/test-workflow-name ]; then
        print_error "워크플로우 이름을 찾을 수 없습니다."
        return 1
    fi

    WORKFLOW_NAME=$(cat /tmp/test-workflow-name)

    echo "Insight Trace 사이드카 로그 (train step):"
    echo "----------------------------------------"

    # train step의 Pod 찾기
    TRAIN_POD=$(kubectl get pods -l "workflows.argoproj.io/workflow=$WORKFLOW_NAME" -o jsonpath='{.items[?(@.metadata.name contains "train")].metadata.name}' 2>/dev/null | head -1)

    if [ -z "$TRAIN_POD" ]; then
        # 모든 워크플로우 Pod 나열
        echo "워크플로우 Pod 목록:"
        kubectl get pods -l "workflows.argoproj.io/workflow=$WORKFLOW_NAME"

        TRAIN_POD=$(kubectl get pods -l "workflows.argoproj.io/workflow=$WORKFLOW_NAME" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    fi

    if [ -n "$TRAIN_POD" ]; then
        echo "Pod: $TRAIN_POD"
        echo ""
        kubectl logs "$TRAIN_POD" -c insight-trace --tail=50 2>/dev/null || echo "(로그 없음 - 아직 실행 전이거나 종료됨)"
    fi
}

# 6. Signature API 확인
check_signature_api() {
    print_header "6. Signature API 확인"

    if [ ! -f /tmp/test-workflow-name ]; then
        print_error "워크플로우 이름을 찾을 수 없습니다."
        return 1
    fi

    WORKFLOW_NAME=$(cat /tmp/test-workflow-name)

    # 실행 중인 Pod 찾기
    RUNNING_POD=$(kubectl get pods -l "workflows.argoproj.io/workflow=$WORKFLOW_NAME" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -z "$RUNNING_POD" ]; then
        print_warning "실행 중인 Pod가 없습니다. (워크플로우가 이미 완료되었을 수 있음)"
        return 0
    fi

    echo "Pod: $RUNNING_POD"
    echo ""

    # Port-forward로 API 접근
    echo "Port-forward 설정 중..."
    kubectl port-forward "$RUNNING_POD" 9090:9090 &>/dev/null &
    PF_PID=$!
    sleep 2

    if kill -0 $PF_PID 2>/dev/null; then
        echo "Signature API 응답:"
        echo "----------------------------------------"
        curl -s http://localhost:9090/signature 2>/dev/null | python3 -m json.tool 2>/dev/null || curl -s http://localhost:9090/signature
        echo ""

        # Port-forward 종료
        kill $PF_PID 2>/dev/null
    else
        print_warning "Port-forward 실패"
    fi
}

# 7. 정리
cleanup() {
    print_header "7. 정리"

    read -p "테스트 워크플로우를 삭제하시겠습니까? (y/N): " -n 1 -r
    echo ""

    if [[ $REPLY =~ ^[Yy]$ ]]; then
        if [ -f /tmp/test-workflow-name ]; then
            WORKFLOW_NAME=$(cat /tmp/test-workflow-name)
            kubectl delete workflow "$WORKFLOW_NAME" 2>/dev/null && print_success "워크플로우 삭제됨"
            rm /tmp/test-workflow-name
        fi
    else
        if [ -f /tmp/test-workflow-name ]; then
            WORKFLOW_NAME=$(cat /tmp/test-workflow-name)
            echo ""
            echo "수동 삭제 명령어:"
            echo "  kubectl delete workflow $WORKFLOW_NAME"
        fi
    fi
}

# 메인 실행
main() {
    print_header "Argo + Insight Trace 연동 테스트"

    case "${1:-all}" in
        check)
            check_argo
            check_insight_trace
            ;;
        deploy)
            deploy_test_workflow
            ;;
        status)
            check_workflow_status
            ;;
        logs)
            check_insight_trace_logs
            ;;
        signature)
            check_signature_api
            ;;
        cleanup)
            cleanup
            ;;
        all)
            check_argo
            check_insight_trace
            deploy_test_workflow
            sleep 5
            check_workflow_status
            check_insight_trace_logs
            check_signature_api
            cleanup
            ;;
        *)
            echo "Usage: $0 {check|deploy|status|logs|signature|cleanup|all}"
            echo ""
            echo "Commands:"
            echo "  check     - Argo/Insight Trace 설치 확인"
            echo "  deploy    - 테스트 워크플로우 배포"
            echo "  status    - 워크플로우 상태 확인"
            echo "  logs      - Insight Trace 로그 확인"
            echo "  signature - Signature API 확인"
            echo "  cleanup   - 테스트 리소스 정리"
            echo "  all       - 전체 테스트 실행 (기본값)"
            exit 1
            ;;
    esac
}

main "$@"
