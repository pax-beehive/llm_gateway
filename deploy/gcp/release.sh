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
schema_migrate_digest=$(resolve_digest schema-migrate)
role_config_digest=$(resolve_digest role-config)

schema_migrate_image="$GCP_ARTIFACT_REPOSITORY/schema-migrate@$schema_migrate_digest"
role_config_image="$GCP_ARTIFACT_REPOSITORY/role-config@$role_config_digest"

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

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    printf 'build_id=%s\n' "$build_id"
    printf 'gateway_digest=%s\n' "$gateway_digest"
    printf 'control_plane_digest=%s\n' "$control_plane_digest"
    printf 'metering_digest=%s\n' "$metering_digest"
    printf 'schema_migrate_digest=%s\n' "$schema_migrate_digest"
    printf 'role_config_digest=%s\n' "$role_config_digest"
    printf 'migration_execution=%s\n' "$migration_execution"
    printf 'role_config_execution=%s\n' "$role_config_execution"
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
    echo "- Schema migration: \`$schema_migrate_digest\`"
    echo "- Role configuration: \`$role_config_digest\`"
    echo "- Migration execution: \`$migration_execution\`"
    echo "- Role configuration execution: \`$role_config_execution\`"
    echo
    echo 'Long-running Cloud Run services and public traffic are outside this release stage.'
  } >>"$GITHUB_STEP_SUMMARY"
fi

echo "GCP release gates completed for $SOURCE_REVISION"
