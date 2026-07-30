<script lang="ts">
  import IssueStatusBadge from '$lib/components/issues/IssueStatusBadge.svelte';
  import IssueAssigneePicker from '$lib/components/issues/IssueAssigneePicker.svelte';

  export let data: any;

  // Mock data for display
  let issue = {
    id: 'ISSUE-123',
    title: 'TypeError: Cannot read property "id" of undefined',
    status: 'unresolved' as const,
    assignee: null,
    createdAt: new Date().toISOString(),
    errorDetails: {
      stackTrace: 'Error\n  at Object.<anonymous> (/app/index.js:10:15)\n  at Module._compile (internal/modules/cjs/loader.js:1063:30)',
      browser: 'Chrome 109',
      os: 'Mac OS X 10.15.7'
    }
  };

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

  let relatedIssues = [
    { id: 'ISSUE-120', title: 'TypeError in user profile' },
    { id: 'ISSUE-115', title: 'Cannot save settings' }
  ];
</script>

<div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
  <div class="bg-white shadow overflow-hidden sm:rounded-lg">
    <div class="px-4 py-5 sm:px-6 flex justify-between items-center">
      <div>
        <h3 class="text-lg leading-6 font-medium text-gray-900">
          {issue.title}
        </h3>
        <p class="mt-1 max-w-2xl text-sm text-gray-500">
          {issue.id} • Created at {new Date(issue.createdAt).toLocaleString()}
        </p>
      </div>
      <div class="flex items-center space-x-4">
        <IssueStatusBadge status={issue.status} />
        <IssueAssigneePicker assignee={issue.assignee} />
      </div>
    </div>
    <div class="border-t border-gray-200 px-4 py-5 sm:p-0">
      <dl class="sm:divide-y sm:divide-gray-200">
        <div class="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
          <dt class="text-sm font-medium text-gray-500">Error Details</dt>
          <dd class="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
            <p><strong>Browser:</strong> {issue.errorDetails.browser}</p>
            <p><strong>OS:</strong> {issue.errorDetails.os}</p>
            <div class="mt-2 p-4 bg-gray-900 text-gray-100 rounded-md overflow-x-auto">
              <pre><code>{issue.errorDetails.stackTrace}</code></pre>
            </div>
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
          <dt class="text-sm font-medium text-gray-500">Related Issues</dt>
          <dd class="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
            <ul class="border border-gray-200 rounded-md divide-y divide-gray-200">
              {#each relatedIssues as rel}
                <li class="pl-3 pr-4 py-3 flex items-center justify-between text-sm">
                  <div class="w-0 flex-1 flex items-center">
                    <span class="ml-2 flex-1 w-0 truncate">
                      <strong>{rel.id}</strong>: {rel.title}
                    </span>
                  </div>
                </li>
              {/each}
            </ul>
          </dd>
        </div>
      </dl>
    </div>
  </div>
</div>
