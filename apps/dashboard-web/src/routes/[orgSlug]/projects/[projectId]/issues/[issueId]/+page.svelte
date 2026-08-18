<script lang="ts">
  import IssueStatusBadge from '$lib/components/issues/IssueStatusBadge.svelte';
  import IssueAssigneePicker from '$lib/components/issues/IssueAssigneePicker.svelte';
  import IssueRelations from '$lib/components/issues/IssueRelations.svelte';
  import CommentThread from '$lib/components/issues/CommentThread.svelte';
  import SubscriptionToggle from '$lib/components/notifications/SubscriptionToggle.svelte';
  import { filterKnownRelationTypes } from '$lib/types/relation-type';
  import type { PageData } from './$types';

  export let data: PageData;

  $: issue = data.issue;

  const KNOWN_STATUSES = ['unresolved', 'resolved', 'ignored'] as const;
  type KnownStatus = (typeof KNOWN_STATUSES)[number];
  function isKnownStatus(value: string): value is KnownStatus {
    return (KNOWN_STATUSES as readonly string[]).includes(value);
  }
  $: statusForBadge = isKnownStatus(issue.status) ? issue.status : 'unresolved';

  function isKnownAssigneeType(value: string | null): value is 'user' | 'agent' {
    return value === 'user' || value === 'agent';
  }
  $: assigneeForPicker = (() => {
    if (!issue.assignedTo || !isKnownAssigneeType(issue.assigneeType)) return null;
    const type: 'user' | 'agent' = issue.assigneeType;
    return { type, id: issue.assignedTo, name: issue.assignedTo };
  })();

  // The DB column is a plain varchar with no DB-level enum, so relations read back out have
  // relationType typed only as `string`. Narrow at this boundary rather than casting (see D18 /
  // relation-type.ts) — unrecognized values are dropped and logged, never rendered.
  $: relationsForPanel = filterKnownRelationTypes(data.relations || []);

  // PLACEHOLDER DATA — this page does not read issue_activity yet. `type` values must use the DB
  // vocabulary so they stay correct when it does: issue_activity.event_type is constrained to
  // status_changed | assigned | unassigned | regressed | ai_analysis | linked. 'status_change' (no
  // 'd') was the same typo that made every real status-change insert fail its CHECK constraint before
  // queries/issues.ts was corrected. Note 'created' below is NOT a permitted event_type — there is no
  // creation row in issue_activity — so the "created this issue" branch will never match real data.
  let activities = [
    { type: 'created', user: 'System', time: '2 hours ago' },
    { type: 'status_changed', user: 'Alice', time: '1 hour ago', detail: 'Changed status to Investigating' }
  ];

  let mutationError: string | null = null;

  // D03: this PATCH used to fire-and-reload with no res.ok check, so a failed status update was
  // invisible — the page just refreshed and silently kept the old status. Surface the failure.
  async function handleStatusChangeRequest(newStatus: 'resolved') {
    mutationError = null;
    try {
      const res = await fetch(`/api/issues/${issue.id}/status`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status: newStatus }),
      });

      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.message || `Failed to update status (${res.status})`);
      }

      window.location.reload();
    } catch (err: any) {
      mutationError = err?.message || 'Failed to update status';
      console.error('Failed to update status:', err);
    }
  }
</script>

<div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
  {#if mutationError}
    <div class="mb-4 rounded-md bg-red-50 border border-red-200 px-4 py-3 text-sm text-red-700" role="alert">
      {mutationError}
    </div>
  {/if}
  <div class="bg-white shadow overflow-hidden sm:rounded-lg">
    <div class="px-4 py-5 sm:px-6 flex justify-between items-center">
      <div>
        <h3 class="text-lg leading-6 font-medium text-gray-900">
          {issue.errorClass}
        </h3>
        <p class="mt-1 max-w-2xl text-sm text-gray-500">
          {issue.id} • First seen at {issue.firstSeen ? new Date(issue.firstSeen).toLocaleString() : 'unknown'}
        </p>
      </div>
      <div class="flex items-center space-x-4">
        <IssueStatusBadge status={statusForBadge} />
        <IssueAssigneePicker assignee={assigneeForPicker} />
        <SubscriptionToggle issueId={issue.id} />
      </div>
    </div>
    <div class="border-t border-gray-200 px-4 py-5 sm:p-0">
      <dl class="sm:divide-y sm:divide-gray-200">
        <div class="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
          <dt class="text-sm font-medium text-gray-500">Error Details</dt>
          <dd class="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
            <p><strong>Message:</strong> {issue.message}</p>
            <p><strong>Fingerprint:</strong> {issue.fingerprint}</p>
          </dd>
        </div>

        <div class="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
          <dt class="text-sm font-medium text-gray-500">Activity Timeline</dt>
          <dd class="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
            <ul class="space-y-4">
              {#each activities as activity}
                <li class="flex space-x-3">
                  <div class="flex-1 space-y-1">
                    <div class="flex items-center justify-between">
                      <h3 class="text-sm font-medium">{activity.user} {activity.type === 'created' ? 'created this issue' : 'updated this issue'}</h3>
                      <p class="text-sm text-gray-500">{activity.time}</p>
                    </div>
                    {#if activity.detail}
                      <p class="text-sm text-gray-500">{activity.detail}</p>
                    {/if}
                  </div>
                </li>
              {/each}
            </ul>
          </dd>
        </div>

        <div class="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
          <dt class="text-sm font-medium text-gray-500">Related Issues & Duplicates</dt>
          <dd class="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
            <IssueRelations
              currentIssueId={issue.id}
              initialRelations={relationsForPanel}
              onStatusChangeRequest={handleStatusChangeRequest}
            />
          </dd>
        </div>
      </dl>
    </div>
  </div>

  <div class="mt-6 bg-white shadow overflow-hidden sm:rounded-lg px-4 py-5 sm:px-6">
    <h3 class="text-lg leading-6 font-medium text-gray-900 mb-4">Discussion</h3>
    {#if data.organizationId}
      <CommentThread
        issueId={issue.id}
        organizationId={data.organizationId}
        currentUserId={data.userId}
        currentUserRole={data.userRole}
      />
    {/if}
  </div>
</div>
