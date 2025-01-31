package clusterinstall

const installTemplateE2E = `
kind: Template
apiVersion: template.openshift.io/v1

parameters:
- name: JOB_NAME_SAFE
  required: true
- name: JOB_NAME_HASH
  required: true
- name: NAMESPACE
  required: true
- name: IMAGE_FORMAT
- name: IMAGE_INSTALLER
  required: true
- name: IMAGE_TESTS
  required: true
- name: CLUSTER_TYPE
  required: true
- name: TEST_COMMAND
  required: true
- name: RELEASE_IMAGE_LATEST
  required: true
- name: BASE_DOMAIN
- name: CLUSTER_NETWORK_MANIFEST
- name: CLUSTER_NETWORK_TYPE
- name: ENABLE_FIPS
- name: ENABLE_PROXY
- name: BUILD_ID
  required: false

objects:

# We want the cluster to be able to access these images
- kind: RoleBinding
  apiVersion: authorization.openshift.io/v1
  metadata:
    name: ${JOB_NAME_SAFE}-image-puller
    namespace: ${NAMESPACE}
  roleRef:
    name: system:image-puller
  subjects:
  - kind: SystemGroup
    name: system:unauthenticated
  - kind: SystemGroup
    name: system:authenticated

# Give admin access to a known bot
- kind: RoleBinding
  apiVersion: authorization.openshift.io/v1
  metadata:
    name: ${JOB_NAME_SAFE}-namespace-admins
    namespace: ${NAMESPACE}
  roleRef:
    name: admin
  subjects:
  - kind: ServiceAccount
    namespace: ci
    name: ci-chat-bot

# The e2e pod spins up a cluster, runs e2e tests, and then cleans up the cluster.
- kind: Pod
  apiVersion: v1
  metadata:
    name: ${JOB_NAME_SAFE}
    namespace: ${NAMESPACE}
    annotations:
      # we want to gather the teardown logs no matter what
      ci-operator.openshift.io/wait-for-container-artifacts: teardown
      ci-operator.openshift.io/save-container-logs: "true"
      ci-operator.openshift.io/container-sub-tests: "lease,setup,test,teardown"
  spec:
    restartPolicy: Never
    activeDeadlineSeconds: 14400
    terminationGracePeriodSeconds: 900
    volumes:
    - name: artifacts
      emptyDir: {}
    - name: shared-tmp
      emptyDir: {}
    - name: cluster-profile
      secret:
        secretName: ${JOB_NAME_SAFE}-cluster-profile

    containers:

    - name: lease
      image: registry.svc.ci.openshift.org/ci/boskoscli:latest
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        requests:
          cpu: 10m
          memory: 10Mi
        limits:
          memory: 200Mi
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp/shared
      - name: cluster-profile
        mountPath: /tmp/cluster
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: CLUSTER_TYPE
        value: ${CLUSTER_TYPE}
      - name: CLUSTER_NAME
        value: ${NAMESPACE}-${JOB_NAME_HASH}
      command:
      - /bin/bash
      - -c
      - |
        #!/bin/bash
        set -euo pipefail

        trap 'rc=$?; CHILDREN=$(jobs -p); if test -n "${CHILDREN}"; then kill ${CHILDREN} && wait; fi; if test "${rc}" -ne 0; then touch /tmp/shared/exit; fi; exit "${rc}"' EXIT

        # hack for bazel
        function boskosctl() {
          /app/boskos/cmd/cli/app.binary "${@}"
        }

        echo "[INFO] Acquiring a lease ..."
        resource="$( boskosctl --server-url http://boskos.ci --owner-name "${CLUSTER_NAME}" acquire --type "${CLUSTER_TYPE}-quota-slice" --state free --target-state leased --timeout 150m )"
        touch /tmp/shared/leased
        echo "[INFO] Lease acquired!"
        echo "[INFO] Leased resource: ${resource}"

        function release() {
            local resource_name; resource_name="$( jq .name --raw-output <<<"${resource}" )"
            echo "[INFO] Releasing the lease on resouce ${resource_name}..."
            boskosctl --server-url http://boskos.ci --owner-name "${CLUSTER_NAME}" release --name "${resource_name}" --target-state free
        }
        trap release EXIT

        echo "[INFO] Sending heartbeats to retain the lease..."
        boskosctl --server-url http://boskos.ci --owner-name "${CLUSTER_NAME}" heartbeat --resource "${resource}" &

        while true; do
          if [[ -f /tmp/shared/exit ]]; then
            echo "Another process exited" 2>&1
            exit 0
          fi

          sleep 15 & wait $!
        done

    # Once the cluster is up, executes shared tests
    - name: test
      image: ${IMAGE_TESTS}
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        requests:
          cpu: 3
          memory: 600Mi
        limits:
          memory: 4Gi
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp/shared
      - name: cluster-profile
        mountPath: /tmp/cluster
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: AWS_SHARED_CREDENTIALS_FILE
        value: /tmp/cluster/.awscred
      - name: AZURE_AUTH_LOCATION
        value: /tmp/cluster/osServicePrincipal.json
      - name: GCP_SHARED_CREDENTIALS_FILE
        value: /tmp/cluster/gce.json
      - name: ARTIFACT_DIR
        value: /tmp/artifacts
      - name: HOME
        value: /tmp/home
      - name: IMAGE_FORMAT
        value: ${IMAGE_FORMAT}
      - name: KUBECONFIG
        value: /tmp/artifacts/installer/auth/kubeconfig
      command:
      - /bin/bash
      - -c
      - |
        #!/bin/bash
        set -euo pipefail

        export PATH=/usr/libexec/origin:$PATH

        trap 'touch /tmp/shared/exit' EXIT
        trap 'kill $(jobs -p); exit 0' TERM

        mkdir -p "${HOME}"

        # wait for the API to come up
        while true; do
          if [[ -f /tmp/shared/exit ]]; then
            echo "Another process exited" 2>&1
            exit 1
          fi
          if [[ ! -f /tmp/shared/setup-success ]]; then
            sleep 15 & wait
            continue
          fi
          # don't let clients impact the global kubeconfig
          cp "${KUBECONFIG}" /tmp/admin.kubeconfig
          export KUBECONFIG=/tmp/admin.kubeconfig
          break
        done

        # if the cluster profile included an insights secret, install it to the cluster to
        # report support data from the support-operator
        if [[ -f /tmp/cluster/insights-live.yaml ]]; then
          oc create -f /tmp/cluster/insights-live.yaml || true
        fi

        # set up cloud-provider-specific env vars
        export KUBE_SSH_BASTION="$( oc --insecure-skip-tls-verify get node -l node-role.kubernetes.io/master -o 'jsonpath={.items[0].status.addresses[?(@.type=="ExternalIP")].address}' ):22"
        export KUBE_SSH_KEY_PATH=/tmp/cluster/ssh-privatekey
        if [[ "${CLUSTER_TYPE}" == "gcp" ]]; then
          export GOOGLE_APPLICATION_CREDENTIALS="${GCP_SHARED_CREDENTIALS_FILE}"
          export KUBE_SSH_USER=core
          mkdir -p ~/.ssh
          cp /tmp/cluster/ssh-privatekey ~/.ssh/google_compute_engine || true
          export TEST_PROVIDER='{"type":"gce","region":"us-east1","multizone": true,"multimaster":true,"projectid":"openshift-gce-devel-ci"}'
        elif [[ "${CLUSTER_TYPE}" == "aws" ]]; then
          mkdir -p ~/.ssh
          cp /tmp/cluster/ssh-privatekey ~/.ssh/kube_aws_rsa || true
          export PROVIDER_ARGS="-provider=aws -gce-zone=us-east-1"
          # TODO: make openshift-tests auto-discover this from cluster config
          export TEST_PROVIDER='{"type":"aws","region":"us-east-1","zone":"us-east-1a","multizone":true,"multimaster":true}'
          export KUBE_SSH_USER=core
        elif [[ "${CLUSTER_TYPE}" == "azure4" ]]; then
          export TEST_PROVIDER='azure'
        fi

        # create fips enable helper
        function enable_fips() {
            for name in $(oc get machineconfigpool --template='{{range .items}}{{.metadata.name}}{{"\n"}}{{end}}')
            do
              cat > /tmp/fips-mc.yaml <<'EOF'
        apiVersion: machineconfiguration.openshift.io/v1
        kind: MachineConfig
        metadata:
          labels:
            machineconfiguration.openshift.io/role: "{{name}}"
          name: 99-fips-"{{name}}"
        spec:
          fips: true
        EOF
              sed -i "s/\"{{name}}\"/${name}/g" /tmp/fips-mc.yaml
              oc create -f /tmp/fips-mc.yaml
            done

            for i in $(seq 0 10); do oc wait machineconfigpool --all --for=condition=Updating --timeout=5m && break; done
            for i in $(seq 0 10); do oc wait machineconfigpool --all --for=condition=Updated --timeout=5m && break; sleep 30; done
        }

        if [[ "${ENABLE_FIPS}" == true ]]; then
          enable_fips
        fi

        mkdir -p /tmp/output
        cd /tmp/output

        function retry() {
          local RETRY_IGNORE_EXIT_CODE="${RETRY_IGNORE_EXIT_CODE:-}"
          local ATTEMPTS="${1}"
          local rc=0
          shift
          for i in $(seq 0 $((ATTEMPTS-1))); do
            echo "--> ${@}"
            set +e
            "${@}"
            rc="$?"
            set -e
            echo "--> exit code: $rc"
            test "${rc}" = 0 && break
            sleep 10
          done
          if [ "${RETRY_IGNORE_EXIT_CODE}" != "" ]; then return 0; else return "${rc}"; fi
        }

        function setup_ssh_bastion() {
          echo "Setting up ssh bastion"
          mkdir -p ~/.ssh || true
          cp "${KUBE_SSH_KEY_PATH}" ~/.ssh/id_rsa
          chmod 0600 ~/.ssh/id_rsa
          if ! whoami &> /dev/null; then
            if [ -w /etc/passwd ]; then
              echo "${USER_NAME:-default}:x:$(id -u):0:${USER_NAME:-default} user:${HOME}:/sbin/nologin" >> /etc/passwd
            fi
          fi
          curl https://raw.githubusercontent.com/eparis/ssh-bastion/master/deploy/deploy.sh | bash
          for i in $(seq 0 60)
          do
            BASTION_HOST=$(oc get service -n openshift-ssh-bastion ssh-bastion -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
            if [ ! -z "${BASTION_HOST}" ]; then break; fi
            sleep 10
          done
        }

        function bastion_ssh() {
          retry 60 \
            ssh -o LogLevel=error -o ConnectionAttempts=100 -o ConnectTimeout=30 -o StrictHostKeyChecking=no \
                -o ProxyCommand="ssh -A -o StrictHostKeyChecking=no -o LogLevel=error -o ServerAliveInterval=30 -o ConnectionAttempts=100 -o ConnectTimeout=30 -W %h:%p core@${BASTION_HOST} 2>/dev/null" \
                $@
        }

        function restore-cluster-state() {
          echo "Placing file /etc/rollback-test with contents A"
          cat > /tmp/machineconfig.yaml <<'EOF'
        apiVersion: machineconfiguration.openshift.io/v1
        kind: MachineConfig
        metadata:
          labels:
            machineconfiguration.openshift.io/role: master
          name: 99-rollback-test
        spec:
          config:
            ignition:
              version: 2.2.0
            storage:
              files:
              - contents:
                  source: data:,A
                filesystem: root
                mode: 420
                path: /etc/rollback-test
        EOF
          oc create -f /tmp/machineconfig.yaml

          function wait_for_machineconfigpool_to_apply() {
            for i in $(seq 0 10); do oc wait machineconfigpool/master --for=condition=Updating --timeout=5m && break; done
            for i in $(seq 0 10); do oc wait machineconfigpool/master --for=condition=Updated --timeout=5m && break; sleep 30; done
          }

          wait_for_machineconfigpool_to_apply

          setup_ssh_bastion

          echo "Make etcd backup on first master - /usr/local/bin/etcd-snapshot-backup.sh"
          FIRST_MASTER=$(oc get node -l node-role.kubernetes.io/master= -o name | head -n1 | cut -d '/' -f 2)
          bastion_ssh "core@${FIRST_MASTER}" "sudo -i /bin/bash -x /usr/local/bin/etcd-snapshot-backup.sh /root/assets/backup/snapshot.db && sudo -i cp /root/assets/backup/snapshot.db /tmp/snapshot.db && sudo -i chown core:core /tmp/snapshot.db"

          # TODO: upgrade conditionally here

          echo "Update rollback-test machineconfig"
          oc patch machineconfig 99-rollback-test -n openshift-machine-api --patch '{"spec":{"config":{"storage":{"files":[{"contents":{"source":"data:,B","verification":{}},"filesystem":"root","mode":420,"path":"/etc/rollback-test"}]}}}}' --type=merge
          wait_for_machineconfigpool_to_apply

          echo "Distribute snapshot across all masters"
          mapfile -t MASTERS < <(oc get node -l node-role.kubernetes.io/master= -o name | cut -d '/' -f 2)
          for master in "${MASTERS[@]}"
          do
            scp -o StrictHostKeyChecking=no -o ProxyCommand="ssh -A -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -W %h:%p core@${BASTION_HOST}" ${KUBE_SSH_KEY_PATH} "core@${master}":/home/core/.ssh/id_rsa
            bastion_ssh "core@${master}" "sudo -i chmod 0600 /home/core/.ssh/id_rsa"
            bastion_ssh "core@${FIRST_MASTER}" "scp -o StrictHostKeyChecking=no /tmp/snapshot.db core@${master}:/tmp/snapshot.db"
          done

          echo "Collect etcd names"
          for master in "${MASTERS[@]}"
          do
            bastion_ssh "core@${master}" 'echo "etcd-member-$(hostname -f)" > /tmp/etcd_name && source /run/etcd/environment && echo "https://${ETCD_DNS_NAME}:2380" > /tmp/etcd_uri'
            bastion_ssh "core@${FIRST_MASTER}" "mkdir -p /tmp/etcd/${master} && scp -o StrictHostKeyChecking=no core@${master}:/tmp/etcd_name /tmp/etcd/${master}/etcd_name && scp -o StrictHostKeyChecking=no core@${master}:/tmp/etcd_uri /tmp/etcd/${master}/etcd_uri"
            bastion_ssh "core@${FIRST_MASTER}" "cat /tmp/etcd/${master}/etcd_name"
            bastion_ssh "core@${FIRST_MASTER}" "cat /tmp/etcd/${master}/etcd_uri"
          done

          echo "Assemble etcd connection string"
          bastion_ssh "core@${FIRST_MASTER}" 'rm -rf /tmp/etcd/connstring && mapfile -t MASTERS < <(ls /tmp/etcd) && echo ${MASTERS[@]} && for master in "${MASTERS[@]}"; do echo -n "$(cat /tmp/etcd/${master}/etcd_name)=$(cat /tmp/etcd/${master}/etcd_uri)," >> /tmp/etcd/connstring; done && sed -i '"'$ s/.$//'"' /tmp/etcd/connstring'

          echo "Restore etcd cluster from snapshot"
          for master in "${MASTERS[@]}"
          do
            echo "Running /usr/local/bin/etcd-snapshot-restore.sh on ${master}"
            bastion_ssh "core@${FIRST_MASTER}" "scp -o StrictHostKeyChecking=no /tmp/etcd/connstring core@${master}:/tmp/etcd_connstring"
            bastion_ssh "core@${master}" 'sudo -i /bin/bash -x /usr/local/bin/etcd-snapshot-restore.sh /tmp/snapshot.db $(cat /tmp/etcd_connstring)'
          done

          echo "Wait for API server to come up"
          for i in $(seq 0 10)
          do
            oc get nodes && break
            sleep 30
          done

          echo "Wait for MCO to rollout new configs"
          for i in $(seq 0 10); do oc get machineconfigpool/master > /dev/null && break; sleep 30; done
          wait_for_machineconfigpool_to_apply

          echo "Wait for all kube-apiserver pods to come back"
          for master in ${MASTERS[@]}
          do
            oc get pod/kube-apiserver-${master} -n openshift-kube-apiserver -o name
            oc wait pod/kube-apiserver-${master} -n openshift-kube-apiserver --for condition=Ready --timeout=5m
          done

          echo "Verify 99-rollback-test machineconfig"
          MC="$(oc get machineconfig/99-rollback-test -o jsonpath='{.spec.config.storage.files[0].contents.source}')"
          if [[ "${MC}" != "data:,A" ]]; then
            echo "Unexpected MachineConfig output: ${MC}"
            exit 1
          fi

          echo "Verify /etc/rollback-test contents"
          rc=0
          for master in "${MASTERS[@]}"
          do
            bastion_ssh core@${master} 'sudo -i test "$(cat /etc/rollback-test)" == "A"'
          done

          if [[ "${rc}" == "1" ]]; then exit 1; fi

          echo "Removing ssh-bastion"
          oc delete project openshift-ssh-bastion

          echo "Remove existing openshift-apiserver pods"
          # This would ensure "Pod 'openshift-apiserver/apiserver-xxx' is not healthy: container openshift-apiserver has restarted more than 5 times" test won't fail
          oc delete pod --all -n openshift-apiserver
        }

        function recover-from-etcd-quorum-loss() {
          setup_ssh_bastion

          # Machine API won't let the user to destroy the node which runs the controller
          echo "Finding two masters to destroy"
          MAPI_POD=$(oc get pod -l k8s-app=controller -n openshift-machine-api --no-headers -o name)
          SURVIVING_MASTER_NODE=$(oc get ${MAPI_POD} -n openshift-machine-api -o jsonpath='{.spec.nodeName}')
          mapfile -t MASTER_NODES_TO_REMOVE < <(oc get nodes -l node-role.kubernetes.io/master= -o name | grep -v "${SURVIVING_MASTER_NODE}")
          MASTER_MACHINES_TO_REMOVE=()
          for master in ${MASTER_NODES_TO_REMOVE[@]}
          do
            MASTER_MACHINES_TO_REMOVE+=($(oc get ${master} -o jsonpath='{.metadata.annotations.machine\.openshift\.io\/machine}' | cut -d '/' -f 2))
          done

          echo "Prepare etcd connstring"
          bastion_ssh "core@${SURVIVING_MASTER_NODE}" 'source /run/etcd/environment && echo "etcd-member-$(hostname -f)=https://${ETCD_DNS_NAME}:2380" > /tmp/etcd_connstring'

          echo "Destroy two masters"
          # Scale down etcd quorum guard
          oc scale --replicas=0 deployment.apps/etcd-quorum-guard -n openshift-machine-config-operator

          for machine in ${MASTER_MACHINES_TO_REMOVE[@]}
          do
            retry 10 oc --request-timeout=5s -n openshift-machine-api delete machine ${machine}
          done

          echo "Confirm meltdown"
          sleep 30
          oc --request-timeout=5s get nodes && exit 1

          echo "Restore single etcd - /usr/local/bin/etcd-snapshot-restore.sh"
          bastion_ssh core@${SURVIVING_MASTER_NODE} 'sudo -i /bin/bash -x /usr/local/bin/etcd-snapshot-restore.sh /root/assets/backup/etcd/member/snap/db $(cat /tmp/etcd_connstring)'

          echo "Wait for API server to come up"
          retry 30 oc get nodes

          # Workaround for https://bugzilla.redhat.com/show_bug.cgi?id=1707006
          echo "Restart SDN"
          retry 10 oc delete pods -l app=sdn -n openshift-sdn --wait=false

          echo "Create two masters via Machine API"
          retry 10 oc get machines -n openshift-machine-api
          # Clone existing masters, update IDs and oc apply
          SURVIVING_MASTER_MACHINE=$(oc get machine -l machine.openshift.io/cluster-api-machine-role=master -n openshift-machine-api -o name | cut -d '/' -f 2)
          SURVIVING_MASTER_NUM=${SURVIVING_MASTER_MACHINE##*-}
          SURVIVING_MASTER_PREFIX=${SURVIVING_MASTER_MACHINE%-*}
          retry 10 sh -c 'oc get --export machine ${SURVIVING_MASTER_MACHINE} -n openshift-machine-api -o yaml > /tmp/machine.template'

          MASTER_INDEX=0
          for i in $(seq 0 1); do
            if [[ "${MASTER_INDEX}" == "${SURVIVING_MASTER_NUM}" ]]; then MASTER_INDEX=$((MASTER_INDEX+1)); fi
            cat /tmp/machine.template \
              | sed 's;selfLink.*;;g' \
              | sed "s;name: ${SURVIVING_MASTER_PREFIX}-${SURVIVING_MASTER_NUM};name: ${SURVIVING_MASTER_PREFIX}-${MASTER_INDEX};" > /tmp/machine_${i}.yaml
              RETRY_IGNORE_EXIT_CODE=1 retry 5 oc create -n openshift-machine-api -f /tmp/machine_${i}.yaml
              MASTER_INDEX=$((MASTER_INDEX+1))
          done

          echo "Waiting for machines to be created"
          set +e
          NEW_MASTER_IPS=()
          for i in $(seq 0 60); do
            NEW_MASTER_IPS=($(oc -n openshift-machine-api \
              get machines \
              -l machine.openshift.io/cluster-api-machine-role=master \
              -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' || true))
            if [[ "${#NEW_MASTER_IPS[@]}" == "3" ]]; then break; fi
            sleep 30
          done
          oc get machines -n openshift-machine-api
          set -e
          if [[ "${#NEW_MASTER_IPS[@]}" != "3" ]]; then
            echo "${NEW_MASTER_IPS[@]}"
            exit 1
          fi

          echo "Verify new master nodes have joined the cluster"
          FOUND_MASTERS=0
          for i in $(seq 1 60)
          do
            FOUND_MASTERS=($(oc --request-timeout=5s get nodes -l node-role.kubernetes.io/master= -o name --no-headers || true))
            if [[ "${#FOUND_MASTERS[@]}" == "3" ]]; then break; fi
            sleep 30
          done
          oc get nodes
          if [[ "${#FOUND_MASTERS[@]}" != "3" ]]; then
            echo "${FOUND_MASTERS[@]}"
            exit 1
          fi

          echo "Update DNS and LB"
          # aws cli magic
          easy_install --user pip
          ~/.local/bin/pip install --user boto3
          cat > /tmp/update_route_53.py <<'PYTHON_EOF'
        import boto3
        import os
        import sys

        if len(sys.argv) < 4:
            print("Usage: ./update_route_53.py <DOMAIN> <RECORD> <IP>")
            sys.exit(1)

        domain = sys.argv[1]
        record = sys.argv[2]
        ip = sys.argv[3]
        print("record: %s" % record)
        print("ip: %s" % ip)

        client = boto3.client('route53')
        r = client.list_hosted_zones_by_name(DNSName=domain, MaxItems="1")
        zone_id = r['HostedZones'][0]['Id'].split('/')[-1]

        response = client.change_resource_record_sets(
            HostedZoneId=zone_id,
            ChangeBatch= {
                'Comment': 'add %s -> %s' % (record, ip),
                'Changes': [
                {
                    'Action': 'UPSERT',
                    'ResourceRecordSet': {
                        'Name': record,
                        'Type': 'A',
                        'TTL': 60,
                        'ResourceRecords': [{'Value': ip}]
                    }
                }]
        })
        PYTHON_EOF
          DOMAIN=$(oc whoami --show-server | grep -oP "api.\\K([^\\:]*)")
          for i in "${!NEW_MASTER_IPS[@]}"; do
            ETCD_NAME="etcd-${i}.${DOMAIN}"
            python /tmp/update_route_53.py "${DOMAIN}" "${ETCD_NAME}" "${NEW_MASTER_IPS[$i]}"
          done

          echo "Run etcd-signer"
          SURVIVING_MASTER_NODE_SHORT=${SURVIVING_MASTER_NODE%%.*}
          curl -O https://raw.githubusercontent.com/hexfusion/openshift-recovery/master/manifests/kube-etcd-cert-signer.yaml.template
          sed "s;__MASTER_HOSTNAME__;${SURVIVING_MASTER_NODE_SHORT};g" kube-etcd-cert-signer.yaml.template > kube-etcd-cert-signer.yaml
          retry 10 oc create -f kube-etcd-cert-signer.yaml
          retry 10 oc get pod/etcd-signer -n openshift-config -o name
          retry 10 oc wait pod/etcd-signer -n openshift-config --for condition=ready

          echo "Grow etcd cluster to full membership"
          SURVIVING_MASTER_IP=$(oc get nodes ${SURVIVING_MASTER_NODE} -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
          SETUP_ETCD_ENVIRONMENT=$(oc adm release info --image-for setup-etcd-environment)
          KUBE_CLIENT_AGENT=$(oc adm release info --image-for kube-client-agent)
          MASTERS=($(oc -n openshift-machine-api \
            get machines \
            -l machine.openshift.io/cluster-api-machine-role=master \
            -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalDNS")].address}{"\n"}{end}'))
          for master in ${MASTERS[@]}
          do
            if [[ "${master}" == ${SURVIVING_MASTER_NODE} ]]; then continue; fi
            echo "Recovering ${master}"
            ETCD_HOSTNAME='etcd-member-$(hostname -f)'
            bastion_ssh core@${master} "sudo -i env  SETUP_ETCD_ENVIRONMENT=${SETUP_ETCD_ENVIRONMENT} KUBE_CLIENT_AGENT=${KUBE_CLIENT_AGENT} /bin/bash -x /usr/local/bin/etcd-member-recover.sh ${SURVIVING_MASTER_IP} ${ETCD_HOSTNAME}"
          done

          for master in ${MASTERS[@]}
          do
            retry 10 oc get pod/etcd-member-${master} -n openshift-etcd -o name
            retry 10 oc wait pod/etcd-member-${master} -n openshift-etcd --for condition=Ready
          done

          echo "Removing ssh-bastion"
          retry 10 oc delete project openshift-ssh-bastion

          echo "Scale etcd-quorum guard"
          retry 10 oc scale --replicas=3 deployment.apps/etcd-quorum-guard -n openshift-machine-config-operator

          echo "Remove etcd-signer"
          oc delete pod/etcd-signer -n openshift-config

          echo "Sleeping for a minute to make sure Prometheus are no longer firing"
          sleep 60
        }

        function setup-google-cloud-sdk() {
          pushd /tmp
          curl -O https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-sdk-256.0.0-linux-x86_64.tar.gz
          tar -xzf google-cloud-sdk-256.0.0-linux-x86_64.tar.gz
          export PATH=$PATH:/tmp/google-cloud-sdk/bin
          mkdir gcloudconfig
          export CLOUDSDK_CONFIG=/tmp/gcloudconfig
          gcloud auth activate-service-account --key-file="${GCP_SHARED_CREDENTIALS_FILE}"
          gcloud config set project openshift-gce-devel-ci
          popd
        }

        function run-dr-snapshot-tests() {
          openshift-tests run-dr restore-snapshot "${TEST_SUITE}" \
            --provider "${TEST_PROVIDER:-}" -o /tmp/artifacts/e2e.log --junit-dir /tmp/artifacts/junit
          return 0
        }

        function run-dr-quorum-tests() {
          openshift-tests run-dr quorum-restore "${TEST_SUITE}" \
            --provider "${TEST_PROVIDER:-}" -o /tmp/artifacts/e2e.log --junit-dir /tmp/artifacts/junit
          return 0
        }

        function run-upgrade-tests() {
          openshift-tests run-upgrade "${TEST_SUITE}" --to-image "${RELEASE_IMAGE_LATEST}" \
            --provider "${TEST_PROVIDER:-}" -o /tmp/artifacts/e2e.log --junit-dir /tmp/artifacts/junit
          return 0
        }

        function run-tests() {
          openshift-tests run "${TEST_SUITE}" \
            --provider "${TEST_PROVIDER:-}" -o /tmp/artifacts/e2e.log --junit-dir /tmp/artifacts/junit
          return 0
        }

        if [[ "${CLUSTER_TYPE}" == "gcp" ]]; then
          setup-google-cloud-sdk
        fi

        ${TEST_COMMAND}

    # Runs an install
    - name: setup
      image: ${IMAGE_INSTALLER}
      terminationMessagePolicy: FallbackToLogsOnError
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp
      - name: cluster-profile
        mountPath: /etc/openshift-installer
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: TYPE
        value: ${CLUSTER_TYPE}
      - name: AWS_SHARED_CREDENTIALS_FILE
        value: /etc/openshift-installer/.awscred
      - name: AWS_REGION
        value: us-east-1
      - name: AZURE_AUTH_LOCATION
        value: /etc/openshift-installer/osServicePrincipal.json
      - name: AZURE_REGION
        value: centralus
      - name: GCP_REGION
        value: us-east1
      - name: GCP_PROJECT
        value: openshift-gce-devel-ci
      - name: GOOGLE_CLOUD_KEYFILE_JSON
        value: /etc/openshift-installer/gce.json
      - name: CLUSTER_NAME
        value: ${NAMESPACE}-${JOB_NAME_HASH}
      - name: BASE_DOMAIN
        value: ${BASE_DOMAIN}
      - name: SSH_PRIV_KEY_PATH
        value: /etc/openshift-installer/ssh-privatekey
      - name: SSH_PUB_KEY_PATH
        value: /etc/openshift-installer/ssh-publickey
      - name: PULL_SECRET_PATH
        value: /etc/openshift-installer/pull-secret
      - name: OPENSHIFT_INSTALL_RELEASE_IMAGE_OVERRIDE
        value: ${RELEASE_IMAGE_LATEST}
      - name: OPENSHIFT_INSTALL_INVOKER
        value: openshift-internal-ci/${JOB_NAME_SAFE}/${BUILD_ID}
      - name: USER
        value: test
      - name: HOME
        value: /tmp
      - name: INSTALL_INITIAL_RELEASE
      - name: RELEASE_IMAGE_INITIAL
      command:
      - /bin/sh
      - -c
      - |
        #!/bin/sh
        trap 'rc=$?; if test "${rc}" -eq 0; then touch /tmp/setup-success; else touch /tmp/exit; fi; exit "${rc}"' EXIT
        trap 'CHILDREN=$(jobs -p); if test -n "${CHILDREN}"; then kill ${CHILDREN} && wait; fi' TERM

        while true; do
          if [[ -f /tmp/exit ]]; then
            echo "Another process exited" 2>&1
            exit 1
          fi
          if [[ -f /tmp/leased ]]; then
            echo "Lease acquired, installing..."
            break
          fi

          sleep 15 & wait
        done

        cp "$(command -v openshift-install)" /tmp
        mkdir /tmp/artifacts/installer

        if [[ -n "${INSTALL_INITIAL_RELEASE}" && -n "${RELEASE_IMAGE_INITIAL}" ]]; then
          echo "Installing from initial release ${RELEASE_IMAGE_INITIAL}"
          OPENSHIFT_INSTALL_RELEASE_IMAGE_OVERRIDE="${RELEASE_IMAGE_INITIAL}"
        else
          echo "Installing from release ${RELEASE_IMAGE_LATEST}"
        fi

        export EXPIRATION_DATE=$(date -d '4 hours' --iso=minutes --utc)
        export SSH_PUB_KEY=$(cat "${SSH_PUB_KEY_PATH}")
        export PULL_SECRET=$(cat "${PULL_SECRET_PATH}")

        ## move private key to ~/.ssh/ so that installer can use it to gather logs on bootstrap failure
        mkdir -p ~/.ssh
        cp "${SSH_PRIV_KEY_PATH}" ~/.ssh/

        if [[ "${CLUSTER_TYPE}" == "aws" ]]; then
            cat > /tmp/artifacts/installer/install-config.yaml << EOF
        apiVersion: v1
        baseDomain: ${BASE_DOMAIN:-origin-ci-int-aws.dev.rhcloud.com}
        metadata:
          name: ${CLUSTER_NAME}
        controlPlane:
          name: master
          replicas: 3
          platform:
            aws:
              zones:
              - us-east-1a
              - us-east-1b
        compute:
        - name: worker
          replicas: 3
          platform:
            aws:
              type: m4.xlarge
              zones:
              - us-east-1a
              - us-east-1b
        platform:
          aws:
            region:       ${AWS_REGION}
            userTags:
              expirationDate: ${EXPIRATION_DATE}
        pullSecret: >
          ${PULL_SECRET}
        sshKey: |
          ${SSH_PUB_KEY}
        EOF
        elif [[ "${CLUSTER_TYPE}" == "azure4" ]]; then
            cat > /tmp/artifacts/installer/install-config.yaml << EOF
        apiVersion: v1
        baseDomain: ${BASE_DOMAIN:-ci.azure.devcluster.openshift.com}
        metadata:
          name: ${CLUSTER_NAME}
        controlPlane:
          name: master
          replicas: 3
        compute:
        - name: worker
          replicas: 3
        platform:
          azure:
            baseDomainResourceGroupName: os4-common
            region: ${AZURE_REGION}
        pullSecret: >
          ${PULL_SECRET}
        sshKey: |
          ${SSH_PUB_KEY}
        EOF
        elif [[ "${CLUSTER_TYPE}" == "gcp" ]]; then
            cat > /tmp/artifacts/installer/install-config.yaml << EOF
        apiVersion: v1
        baseDomain: ${BASE_DOMAIN:-origin-ci-int-gce.dev.openshift.com}
        metadata:
          name: ${CLUSTER_NAME}
        controlPlane:
          name: master
          replicas: 3
        compute:
        - name: worker
          replicas: 3
        platform:
          gcp:
            projectID: ${GCP_PROJECT}
            region: ${GCP_REGION}
        pullSecret: >
          ${PULL_SECRET}
        sshKey: |
          ${SSH_PUB_KEY}
        EOF
        else
            echo "Unsupported cluster type '${CLUSTER_TYPE}'"
            exit 1
        fi

        # as a current stop gap -- this is pointing to a proxy hosted in
        # the namespace "ci-test-ewolinet" on the ci cluster
        if [[ "${ENABLE_PROXY}" == "true" ]]; then
          cat >> /tmp/artifacts/installer/install-config.yaml << EOF
        proxy:
          httpsProxy: https://ewolinet:5f6ccbbbafc66013d012839921ada773@35.231.5.161:3128/
          httpProxy: http://ewolinet:5f6ccbbbafc66013d012839921ada773@35.196.128.173:3128/
        additionalTrustBundle: |
          -----BEGIN CERTIFICATE-----
          MIIC6jCCAdKgAwIBAgIBATANBgkqhkiG9w0BAQsFADAmMSQwIgYDVQQDDBtvcGVu
          c2hpZnQtc2lnbmVyQDE1NjU2NDA4NzYwHhcNMTkwODEyMjAxNDM1WhcNMjQwODEw
          MjAxNDM2WjAmMSQwIgYDVQQDDBtvcGVuc2hpZnQtc2lnbmVyQDE1NjU2NDA4NzYw
          ggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCzbYmY8T9SPUS9c4VG1DNA
          Ub5WKz/NJigFVJ0ei9+mIMF2mHFJlRjHaOs7HaOWTcQNkNBhBfKvNcK8ZKd+kBVB
          CaT9TXOmXlDpaMbnGiGQeBaGrA2S1FzxkQbZDaztN8S3lgydzAVYN7QehRKtP7Zp
          +55gdlw0qvQiepRQaq4RWCgoALY4aJzZRWc/ZTY+wiMURuusC/viVpxhnOrZ5ZkD
          FjnGY+MxB2O4KuSuI6868Sk24ZQ7d9ocRCNRbsinpZTafz9/IpxxoR06PsSNN0NI
          4cpckcTmSysLePTSr+cVvgc9Nr+TJxISC3gtn2U80l/uml1crQ7yfpq7Lf/NPa/b
          AgMBAAGjIzAhMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MA0GCSqG
          SIb3DQEBCwUAA4IBAQCTpgR0rXLZH9WFu9RBxqa7MXdAnmb7hlcKkQHRqVKgTk2N
          z2Hio6l9mHdi42gihWAUKBIwYs2Axk/jjqaI03+CyutZvdnt9N55lsa0qntHuFm5
          jstKn08+IiX6tRRhMqIK27exV0HRbzeAyMDbhjReHnq1OnW/ycyv4p5BdOtuxTox
          8yOmu4a5lKgNfmK5qpE/VsX2jEpqmjck/JaVldcGoICd2DCoMYdHpm7ROFmdTApJ
          WqtDEPIq0PUnMrlr6Ba5GCS3385BWSMvYsbzIiyKXn7hEGh/oFQR2HXix7lYyEyd
          7t6Hv8LhnjP4+HoGlxTSReJ0lXv7mEK0FKXOdkHd
          -----END CERTIFICATE-----
        EOF
        fi

        if [[ -n "${CLUSTER_NETWORK_TYPE}" ]]; then
          cat >> /tmp/artifacts/installer/install-config.yaml << EOF
        networking:
          networkType: ${CLUSTER_NETWORK_TYPE}
        EOF
        fi

        # TODO: Replace with a more concise manifest injection approach
        if [[ -n "${CLUSTER_NETWORK_MANIFEST}" ]]; then
            openshift-install --dir=/tmp/artifacts/installer/ create manifests
            echo "${CLUSTER_NETWORK_MANIFEST}" > /tmp/artifacts/installer/manifests/cluster-network-03-config.yml
        fi

        TF_LOG=debug openshift-install --dir=/tmp/artifacts/installer create cluster &
        wait "$!"

    # Performs cleanup of all created resources
    - name: teardown
      image: ${IMAGE_TESTS}
      terminationMessagePolicy: FallbackToLogsOnError
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp/shared
      - name: cluster-profile
        mountPath: /etc/openshift-installer
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: INSTANCE_PREFIX
        value: ${NAMESPACE}-${JOB_NAME_HASH}
      - name: TYPE
        value: ${CLUSTER_TYPE}
      - name: AWS_SHARED_CREDENTIALS_FILE
        value: /etc/openshift-installer/.awscred
      - name: AWS_REGION
        value: us-east-1
      - name: AZURE_AUTH_LOCATION
        value: /etc/openshift-installer/osServicePrincipal.json
      - name: AZURE_REGION
        value: centralus
      - name: GOOGLE_CLOUD_KEYFILE_JSON
        value: /etc/openshift-installer/gce.json
      - name: KUBECONFIG
        value: /tmp/artifacts/installer/auth/kubeconfig
      command:
      - /bin/bash
      - -c
      - |
        #!/bin/bash
        function queue() {
          local TARGET="${1}"
          shift
          local LIVE="$(jobs | wc -l)"
          while [[ "${LIVE}" -ge 45 ]]; do
            sleep 1
            LIVE="$(jobs | wc -l)"
          done
          echo "${@}"
          if [[ -n "${FILTER}" ]]; then
            "${@}" | "${FILTER}" >"${TARGET}" &
          else
            "${@}" >"${TARGET}" &
          fi
        }

        function teardown() {
          set +e
          touch /tmp/shared/exit
          export PATH=$PATH:/tmp/shared

          echo "Gathering artifacts ..."
          mkdir -p /tmp/artifacts/pods /tmp/artifacts/nodes /tmp/artifacts/metrics /tmp/artifacts/bootstrap /tmp/artifacts/network


          if [ -f /tmp/artifacts/installer/terraform.tfstate ]
          then
              # we don't have jq, so the python equivalent of
              # jq '.modules[].resources."aws_instance.bootstrap".primary.attributes."public_ip" | select(.)'
              bootstrap_ip=$(python -c \
                  'import sys, json; d=reduce(lambda x,y: dict(x.items() + y.items()), map(lambda x: x["resources"], json.load(sys.stdin)["modules"])); k="aws_instance.bootstrap"; print d[k]["primary"]["attributes"]["public_ip"] if k in d else ""' \
                  < /tmp/artifacts/installer/terraform.tfstate
              )

              if [ -n "${bootstrap_ip}" ]
              then
                for service in bootkube openshift kubelet crio
                do
                    queue "/tmp/artifacts/bootstrap/${service}.service" curl \
                        --insecure \
                        --silent \
                        --connect-timeout 5 \
                        --retry 3 \
                        --cert /tmp/artifacts/installer/tls/journal-gatewayd.crt \
                        --key /tmp/artifacts/installer/tls/journal-gatewayd.key \
                        --url "https://${bootstrap_ip}:19531/entries?_SYSTEMD_UNIT=${service}.service"
                done
                if ! whoami &> /dev/null; then
                  if [ -w /etc/passwd ]; then
                    echo "${USER_NAME:-default}:x:$(id -u):0:${USER_NAME:-default} user:${HOME}:/sbin/nologin" >> /etc/passwd
                  fi
                fi
                eval $(ssh-agent)
                ssh-add /etc/openshift-installer/ssh-privatekey
                ssh -A -o PreferredAuthentications=publickey -o StrictHostKeyChecking=false -o UserKnownHostsFile=/dev/null core@${bootstrap_ip} /bin/bash -x /usr/local/bin/installer-gather.sh
                scp -o PreferredAuthentications=publickey -o StrictHostKeyChecking=false -o UserKnownHostsFile=/dev/null core@${bootstrap_ip}:log-bundle.tar.gz /tmp/artifacts/installer/bootstrap-logs.tar.gz
              fi
          else
              echo "No terraform statefile found. Skipping collection of bootstrap logs."
          fi

          oc --insecure-skip-tls-verify --request-timeout=5s get nodes -o jsonpath --template '{range .items[*]}{.metadata.name}{"\n"}{end}' > /tmp/nodes
          oc --insecure-skip-tls-verify --request-timeout=5s get pods --all-namespaces --template '{{ range .items }}{{ $name := .metadata.name }}{{ $ns := .metadata.namespace }}{{ range .spec.containers }}-n {{ $ns }} {{ $name }} -c {{ .name }}{{ "\n" }}{{ end }}{{ range .spec.initContainers }}-n {{ $ns }} {{ $name }} -c {{ .name }}{{ "\n" }}{{ end }}{{ end }}' > /tmp/containers
          oc --insecure-skip-tls-verify --request-timeout=5s get pods -l openshift.io/component=api --all-namespaces --template '{{ range .items }}-n {{ .metadata.namespace }} {{ .metadata.name }}{{ "\n" }}{{ end }}' > /tmp/pods-api

          queue /tmp/artifacts/config-resources.json oc --insecure-skip-tls-verify --request-timeout=5s get apiserver.config.openshift.io authentication.config.openshift.io build.config.openshift.io console.config.openshift.io dns.config.openshift.io featuregate.config.openshift.io image.config.openshift.io infrastructure.config.openshift.io ingress.config.openshift.io network.config.openshift.io oauth.config.openshift.io project.config.openshift.io scheduler.config.openshift.io -o json
          queue /tmp/artifacts/apiservices.json oc --insecure-skip-tls-verify --request-timeout=5s get apiservices -o json
          queue /tmp/artifacts/clusteroperators.json oc --insecure-skip-tls-verify --request-timeout=5s get clusteroperators -o json
          queue /tmp/artifacts/clusterversion.json oc --insecure-skip-tls-verify --request-timeout=5s get clusterversion -o json
          queue /tmp/artifacts/configmaps.json oc --insecure-skip-tls-verify --request-timeout=5s get configmaps --all-namespaces -o json
          queue /tmp/artifacts/credentialsrequests.json oc --insecure-skip-tls-verify --request-timeout=5s get credentialsrequests --all-namespaces -o json
          queue /tmp/artifacts/csr.json oc --insecure-skip-tls-verify --request-timeout=5s get csr -o json
          queue /tmp/artifacts/endpoints.json oc --insecure-skip-tls-verify --request-timeout=5s get endpoints --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/deployments.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get deployments --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/daemonsets.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get daemonsets --all-namespaces -o json
          queue /tmp/artifacts/events.json oc --insecure-skip-tls-verify --request-timeout=5s get events --all-namespaces -o json
          queue /tmp/artifacts/kubeapiserver.json oc --insecure-skip-tls-verify --request-timeout=5s get kubeapiserver -o json
          queue /tmp/artifacts/kubecontrollermanager.json oc --insecure-skip-tls-verify --request-timeout=5s get kubecontrollermanager -o json
          queue /tmp/artifacts/machineconfigpools.json oc --insecure-skip-tls-verify --request-timeout=5s get machineconfigpools -o json
          queue /tmp/artifacts/machineconfigs.json oc --insecure-skip-tls-verify --request-timeout=5s get machineconfigs -o json
          queue /tmp/artifacts/namespaces.json oc --insecure-skip-tls-verify --request-timeout=5s get namespaces -o json
          queue /tmp/artifacts/nodes.json oc --insecure-skip-tls-verify --request-timeout=5s get nodes -o json
          queue /tmp/artifacts/openshiftapiserver.json oc --insecure-skip-tls-verify --request-timeout=5s get openshiftapiserver -o json
          queue /tmp/artifacts/pods.json oc --insecure-skip-tls-verify --request-timeout=5s get pods --all-namespaces -o json
          queue /tmp/artifacts/persistentvolumes.json oc --insecure-skip-tls-verify --request-timeout=5s get persistentvolumes --all-namespaces -o json
          queue /tmp/artifacts/persistentvolumeclaims.json oc --insecure-skip-tls-verify --request-timeout=5s get persistentvolumeclaims --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/replicasets.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get replicasets --all-namespaces -o json
          queue /tmp/artifacts/rolebindings.json oc --insecure-skip-tls-verify --request-timeout=5s get rolebindings --all-namespaces -o json
          queue /tmp/artifacts/roles.json oc --insecure-skip-tls-verify --request-timeout=5s get roles --all-namespaces -o json
          queue /tmp/artifacts/services.json oc --insecure-skip-tls-verify --request-timeout=5s get services --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/statefulsets.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get statefulsets --all-namespaces -o json

          FILTER=gzip queue /tmp/artifacts/openapi.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get --raw /openapi/v2

          # gather nodes first in parallel since they may contain the most relevant debugging info
          while IFS= read -r i; do
            mkdir -p /tmp/artifacts/nodes/$i
            queue /tmp/artifacts/nodes/$i/heap oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/debug/pprof/heap
          done < /tmp/nodes

          FILTER=gzip queue /tmp/artifacts/nodes/masters-journal.gz oc --insecure-skip-tls-verify adm node-logs --role=master --unify=false
          FILTER=gzip queue /tmp/artifacts/nodes/workers-journal.gz oc --insecure-skip-tls-verify adm node-logs --role=worker --unify=false

          # Snapshot iptables-save on each node for debugging possible kube-proxy issues
          oc --insecure-skip-tls-verify get --request-timeout=20s -n openshift-sdn -l app=sdn pods --template '{{ range .items }}{{ .metadata.name }}{{ "\n" }}{{ end }}' > /tmp/sdn-pods
          while IFS= read -r i; do
            queue /tmp/artifacts/network/iptables-save-$i oc --insecure-skip-tls-verify rsh --timeout=20 -n openshift-sdn -c sdn $i iptables-save -c
          done < /tmp/sdn-pods

          while IFS= read -r i; do
            file="$( echo "$i" | cut -d ' ' -f 3 | tr -s ' ' '_' )"
            queue /tmp/artifacts/metrics/${file}-heap oc --insecure-skip-tls-verify exec $i -- /bin/bash -c 'oc --insecure-skip-tls-verify get --raw /debug/pprof/heap --server "https://$( hostname ):8443" --config /etc/origin/master/admin.kubeconfig'
            queue /tmp/artifacts/metrics/${file}-controllers-heap oc --insecure-skip-tls-verify exec $i -- /bin/bash -c 'oc --insecure-skip-tls-verify get --raw /debug/pprof/heap --server "https://$( hostname ):8444" --config /etc/origin/master/admin.kubeconfig'
          done < /tmp/pods-api

          while IFS= read -r i; do
            file="$( echo "$i" | cut -d ' ' -f 2,3,5 | tr -s ' ' '_' )"
            FILTER=gzip queue /tmp/artifacts/pods/${file}.log.gz oc --insecure-skip-tls-verify logs --request-timeout=20s $i
            FILTER=gzip queue /tmp/artifacts/pods/${file}_previous.log.gz oc --insecure-skip-tls-verify logs --request-timeout=20s -p $i
          done < /tmp/containers

          echo "Snapshotting prometheus (may take 15s) ..."
          queue /tmp/artifacts/metrics/prometheus.tar.gz oc --insecure-skip-tls-verify exec -n openshift-monitoring prometheus-k8s-0 -- tar cvzf - -C /prometheus .

          echo "Running must-gather..."
          mkdir -p /tmp/artifacts/must-gather
          queue /tmp/artifacts/must-gather/must-gather.log oc --insecure-skip-tls-verify adm must-gather --dest-dir /tmp/artifacts/must-gather

          echo "Waiting for logs ..."
          wait

          echo "Deprovisioning cluster ..."
          openshift-install --dir /tmp/artifacts/installer destroy cluster
        }

        trap 'teardown' EXIT
        trap 'kill $(jobs -p); exit 0' TERM

        for i in $(seq 1 180); do
          if [[ -f /tmp/shared/exit ]]; then
            exit 0
          fi
          sleep 60 & wait
        done
`
