<script lang="ts">
  export let filters = { status: 'all', assignee: 'all' };
</script>

<div class="flex items-center space-x-4 p-4 bg-gray-50 border-b border-gray-200">
  <div class="flex items-center space-x-2">
    <label for="status-filter" class="text-sm font-medium text-gray-700">Status</label>
    <select id="status-filter" bind:value={filters.status} class="mt-1 block w-full pl-3 pr-10 py-2 text-base border-gray-300 focus:outline-none focus:ring-indigo-500 focus:border-indigo-500 sm:text-sm rounded-md">
      <option value="all">All</option>
      <option value="unresolved">Unresolved</option>
      <option value="resolved">Resolved</option>
    </select>
  </div>
  <div class="flex items-center space-x-2">
    <label for="assignee-filter" class="text-sm font-medium text-gray-700">Assignee</label>
    <select id="assignee-filter" bind:value={filters.assignee} class="mt-1 block w-full pl-3 pr-10 py-2 text-base border-gray-300 focus:outline-none focus:ring-indigo-500 focus:border-indigo-500 sm:text-sm rounded-md">
      <option value="all">All</option>
      <option value="unassigned">Unassigned</option>
      <option value="me">Me</option>
    </select>
  </div>
  <div class="flex-grow"></div>
  <div>
    <input type="text" placeholder="Search issues..." class="shadow-sm focus:ring-indigo-500 focus:border-indigo-500 block w-full sm:text-sm border-gray-300 rounded-md" />
  </div>
</div>
