#!/bin/sh
set -eu

require() {
  variable_name=$1
  variable_value=$2
  if [ -z "$variable_value" ]; then
    echo "$variable_name is required" >&2
    exit 2
  fi
}

require GCP_PROJECT_ID "${GCP_PROJECT_ID:-}"
require GCP_REGION "${GCP_REGION:-}"
require GCP_ARTIFACT_REPOSITORY "${GCP_ARTIFACT_REPOSITORY:-}"
require GCP_BUILD_SERVICE_ACCOUNT "${GCP_BUILD_SERVICE_ACCOUNT:-}"
require GCP_BUILD_SOURCE_BUCKET "${GCP_BUILD_SOURCE_BUCKET:-}"
require SOURCE_REVISION "${SOURCE_REVISION:-}"

case "$SOURCE_REVISION" in
  *[!0-9a-f]*)
    echo "SOURCE_REVISION must be exactly 40 lowercase hexadecimal characters" >&2
    exit 2
    ;;
esac
if [ "${#SOURCE_REVISION}" -ne 40 ]; then
  echo "SOURCE_REVISION must be exactly 40 lowercase hexadecimal characters" >&2
  exit 2
fi

BUILD_POLL_SECONDS=${BUILD_POLL_SECONDS:-15}
MIGRATION_JOB=${MIGRATION_JOB:-llm-gateway-prod-schema-migrate}
ROLE_CONFIG_JOB=${ROLE_CONFIG_JOB:-llm-gateway-prod-role-config}
CONTROL_PLANE_SERVICE=${CONTROL_PLANE_SERVICE:-llm-gateway-prod-control-plane}
METERING_SERVICE=${METERING_SERVICE:-llm-gateway-prod-metering}
GATEWAY_SERVICE=${GATEWAY_SERVICE:-llm-gateway-prod-gateway}
BFF_SERVICE=${BFF_SERVICE:-llm-gateway-prod-console}

case "$GCP_BUILD_SERVICE_ACCOUNT" in
  projects/*/serviceAccounts/*)
    build_service_account_resource=$GCP_BUILD_SERVICE_ACCOUNT
    ;;
  *@*.iam.gserviceaccount.com)
    build_service_account_resource="projects/$GCP_PROJECT_ID/serviceAccounts/$GCP_BUILD_SERVICE_ACCOUNT"
    ;;
  *)
    echo "GCP_BUILD_SERVICE_ACCOUNT must be a service-account email or full resource name" >&2
    exit 2
    ;;
esac

build_id=$(gcloud builds submit . \
  --project="$GCP_PROJECT_ID" \
  --region="$GCP_REGION" \
  --config=deploy/gcp/cloudbuild.yaml \
  --service-account="$build_service_account_resource" \
  --gcs-source-staging-dir="gs://$GCP_BUILD_SOURCE_BUCKET/source" \
  --substitutions="_SOURCE_REVISION=$SOURCE_REVISION,_REPOSITORY=$GCP_ARTIFACT_REPOSITORY" \
  --async \
  --format='value(id)')

if [ -z "$build_id" ]; then
  echo "Cloud Build did not return a build ID" >&2
  exit 1
fi
echo "Cloud Build started: $build_id"

while :; do
  build_status=$(gcloud builds describe "$build_id" \
    --project="$GCP_PROJECT_ID" \
    --region="$GCP_REGION" \
    --format='value(status)')
  case "$build_status" in
    SUCCESS)
      break
      ;;
    PENDING | QUEUED | WORKING)
      sleep "$BUILD_POLL_SECONDS"
      ;;
    *)
      echo "Cloud Build $build_id ended with status $build_status" >&2
      gcloud builds describe "$build_id" \
        --project="$GCP_PROJECT_ID" \
        --region="$GCP_REGION" \
        --format='yaml(id,status,failureInfo)'
      exit 1
      ;;
  esac
done

resolve_digest() {
  image_name=$1
  image_digest=$(gcloud artifacts docker images describe \
    "$GCP_ARTIFACT_REPOSITORY/$image_name:$SOURCE_REVISION" \
    --project="$GCP_PROJECT_ID" \
    --format='value(image_summary.digest)')
  case "$image_digest" in
    sha256:????????????????????????????????????????????????????????????????)
      printf '%s' "$image_digest"
      ;;
    *)
      echo "Artifact Registry returned an invalid digest for $image_name" >&2
      exit 1
      ;;
  esac
}

gateway_digest=$(resolve_digest gateway)
control_plane_digest=$(resolve_digest control-plane)
metering_digest=$(resolve_digest metering)
bff_digest=$(resolve_digest bff)
schema_migrate_digest=$(resolve_digest schema-migrate)
provider_bootstrap_digest=$(resolve_digest provider-bootstrap)
gateway_bootstrap_digest=$(resolve_digest gateway-bootstrap)
role_config_digest=$(resolve_digest role-config)

schema_migrate_image="$GCP_ARTIFACT_REPOSITORY/schema-migrate@$schema_migrate_digest"
role_config_image="$GCP_ARTIFACT_REPOSITORY/role-config@$role_config_digest"
gateway_image="$GCP_ARTIFACT_REPOSITORY/gateway@$gateway_digest"
control_plane_image="$GCP_ARTIFACT_REPOSITORY/control-plane@$control_plane_digest"
metering_image="$GCP_ARTIFACT_REPOSITORY/metering@$metering_digest"
bff_image="$GCP_ARTIFACT_REPOSITORY/bff@$bff_digest"

gcloud run jobs update "$MIGRATION_JOB" \
  --project="$GCP_PROJECT_ID" \
  --region="$GCP_REGION" \
  --image="$schema_migrate_image" \
  --quiet
migration_execution=$(gcloud run jobs execute "$MIGRATION_JOB" \
  --project="$GCP_PROJECT_ID" \
  --region="$GCP_REGION" \
  --wait \
  --format='value(metadata.name)')

gcloud run jobs update "$ROLE_CONFIG_JOB" \
  --project="$GCP_PROJECT_ID" \
  --region="$GCP_REGION" \
  --image="$role_config_image" \
  --quiet
role_config_execution=$(gcloud run jobs execute "$ROLE_CONFIG_JOB" \
  --project="$GCP_PROJECT_ID" \
  --region="$GCP_REGION" \
  --wait \
  --format='value(metadata.name)')

service_exists() {
  gcloud run services describe "$1" \
    --project="$GCP_PROJECT_ID" \
    --region="$GCP_REGION" \
    --format='value(metadata.name)' >/dev/null 2>&1
}

update_service() {
  service_name=$1
  service_image=$2
  echo "Updating private Cloud Run service $service_name" >&2
  gcloud run services update "$service_name" \
    --project="$GCP_PROJECT_ID" \
    --region="$GCP_REGION" \
    --image="$service_image" \
    --quiet \
    --format=none
  latest_created=$(gcloud run services describe "$service_name" \
    --project="$GCP_PROJECT_ID" \
    --region="$GCP_REGION" \
    --format='value(status.latestCreatedRevisionName)')
  latest_ready=$(gcloud run services describe "$service_name" \
    --project="$GCP_PROJECT_ID" \
    --region="$GCP_REGION" \
    --format='value(status.latestReadyRevisionName)')
  if [ -z "$latest_ready" ] || [ "$latest_created" != "$latest_ready" ]; then
    echo "$service_name did not converge to a ready revision" >&2
    exit 1
  fi
  printf '%s' "$latest_ready"
}

control_plane_revision="not-provisioned"
metering_revision="not-provisioned"
gateway_revision="blocked-on-provider-and-routing-bootstrap"
bff_revision="not-provisioned"

if service_exists "$CONTROL_PLANE_SERVICE"; then
  control_plane_revision=$(update_service "$CONTROL_PLANE_SERVICE" "$control_plane_image")
fi
if service_exists "$METERING_SERVICE"; then
  metering_revision=$(update_service "$METERING_SERVICE" "$metering_image")
fi
if service_exists "$GATEWAY_SERVICE"; then
  gateway_revision=$(update_service "$GATEWAY_SERVICE" "$gateway_image")
fi
if service_exists "$BFF_SERVICE"; then
  bff_revision=$(update_service "$BFF_SERVICE" "$bff_image")
fi

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    printf 'build_id=%s\n' "$build_id"
    printf 'gateway_digest=%s\n' "$gateway_digest"
    printf 'control_plane_digest=%s\n' "$control_plane_digest"
    printf 'metering_digest=%s\n' "$metering_digest"
    printf 'bff_digest=%s\n' "$bff_digest"
    printf 'schema_migrate_digest=%s\n' "$schema_migrate_digest"
    printf 'provider_bootstrap_digest=%s\n' "$provider_bootstrap_digest"
    printf 'gateway_bootstrap_digest=%s\n' "$gateway_bootstrap_digest"
    printf 'role_config_digest=%s\n' "$role_config_digest"
    printf 'migration_execution=%s\n' "$migration_execution"
    printf 'role_config_execution=%s\n' "$role_config_execution"
    printf 'control_plane_revision=%s\n' "$control_plane_revision"
    printf 'metering_revision=%s\n' "$metering_revision"
    printf 'gateway_revision=%s\n' "$gateway_revision"
    printf 'bff_revision=%s\n' "$bff_revision"
  } >>"$GITHUB_OUTPUT"
fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo '## GCP release evidence'
    echo
    echo "- Source SHA: \`$SOURCE_REVISION\`"
    echo "- Cloud Build: \`$build_id\`"
    echo "- Gateway: \`$gateway_digest\`"
    echo "- Control Plane: \`$control_plane_digest\`"
    echo "- Metering: \`$metering_digest\`"
    echo "- BFF console: \`$bff_digest\`"
    echo "- Schema migration: \`$schema_migrate_digest\`"
    echo "- Provider bootstrap: \`$provider_bootstrap_digest\`"
    echo "- Gateway bootstrap: \`$gateway_bootstrap_digest\`"
    echo "- Role configuration: \`$role_config_digest\`"
    echo "- Migration execution: \`$migration_execution\`"
    echo "- Role configuration execution: \`$role_config_execution\`"
    echo "- Control Plane revision: \`$control_plane_revision\`"
    echo "- Metering revision: \`$metering_revision\`"
    echo "- Gateway revision: \`$gateway_revision\`"
    echo "- BFF console revision: \`$bff_revision\`"
    echo
    echo 'Gateway, Control Plane, and Metering remain IAM-protected behind internal-and-load-balancer ingress. The WorkOS-authenticated BFF console is public only when separately provisioned by Terraform.'
  } >>"$GITHUB_STEP_SUMMARY"
fi

echo "GCP release gates completed for $SOURCE_REVISION"
