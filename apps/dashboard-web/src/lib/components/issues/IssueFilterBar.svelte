<script lang="ts">
  export let filters = { status: 'all', assignee: 'all' };
</script>

<div class="filter-bar">
  <div class="filter-group">
    <label for="status-filter" class="filter-label">Status</label>
    <select id="status-filter" bind:value={filters.status} class="filter-select">
      <option value="all">All</option>
      <option value="unresolved">Unresolved</option>
      <option value="resolved">Resolved</option>
    </select>
  </div>
  <div class="filter-group">
    <label for="assignee-filter" class="filter-label">Assignee</label>
    <select id="assignee-filter" bind:value={filters.assignee} class="filter-select">
      <option value="all">All</option>
      <option value="unassigned">Unassigned</option>
      <option value="me">Me</option>
    </select>
  </div>
  <div class="filter-spacer"></div>
  <div>
    <input type="text" placeholder="Search issues..." class="filter-search" />
  </div>
</div>

<style>
  /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind.
     The original `bg-gray-50` / `text-gray-700` were light-theme defaults in a dark application. */
  .filter-bar {
    display: flex;
    align-items: center;
    gap: 1rem;
    padding: 1rem;
    background-color: var(--bg-surface);
    border-bottom: 1px solid var(--border-color);
  }

  .filter-group {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .filter-label {
    font-size: 0.8125rem;
    font-weight: 500;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .filter-select {
    padding: 0.375rem 0.625rem;
    font-size: 0.875rem;
  }

  .filter-spacer {
    flex-grow: 1;
  }

  .filter-search {
    padding: 0.375rem 0.625rem;
    font-size: 0.875rem;
    min-width: 14rem;
  }
</style>
