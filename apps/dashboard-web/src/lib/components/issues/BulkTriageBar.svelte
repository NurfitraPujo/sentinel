<script lang="ts">
  export let selectedCount = 0;
  export let onResolve: () => void = () => {};
  export let onIgnore: () => void = () => {};
</script>

{#if selectedCount > 0}
<div class="bulk-bar">
  <div class="bulk-inner">
    <div class="bulk-panel">
      <div class="bulk-row">
        <div class="bulk-message">
          <span class="bulk-icon-wrap">
            <svg class="bulk-icon" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
          </span>
          <p class="bulk-text">
            <span class="bulk-text-short">{selectedCount} selected</span>
            <span class="bulk-text-long">{selectedCount} issues selected for bulk action</span>
          </p>
        </div>
        <div class="bulk-actions">
          <button on:click={onResolve} class="bulk-btn bulk-btn-primary">
            Mark Resolved
          </button>
          <button on:click={onIgnore} class="bulk-btn bulk-btn-secondary">
            Ignore
          </button>
        </div>
      </div>
    </div>
  </div>
</div>
{/if}

<style>
  /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind.
     The responsive show/hide of the two label variants was `md:hidden` / `hidden md:inline`; it is
     a media query here, so the short label no longer renders alongside the long one. */
  .bulk-bar {
    position: fixed;
    bottom: 0;
    left: 0;
    right: 0;
    padding-bottom: 1.25rem;
    z-index: 50;
  }

  .bulk-inner {
    max-width: 80rem;
    margin: 0 auto;
    padding: 0 1rem;
  }

  .bulk-panel {
    padding: 0.75rem;
    border-radius: var(--radius-lg);
    background-color: var(--color-primary);
    box-shadow: 0 10px 25px rgba(0, 0, 0, 0.4);
  }

  .bulk-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-wrap: wrap;
    gap: 0.75rem;
  }

  .bulk-message {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    min-width: 0;
    flex: 1;
  }

  .bulk-icon-wrap {
    display: flex;
    padding: 0.5rem;
    border-radius: var(--radius-md);
    background-color: var(--color-primary-hover);
  }

  .bulk-icon {
    width: 1.5rem;
    height: 1.5rem;
    color: var(--text-primary);
  }

  .bulk-text {
    font-weight: 500;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .bulk-text-long {
    display: none;
  }

  @media (min-width: 768px) {
    .bulk-text-short {
      display: none;
    }

    .bulk-text-long {
      display: inline;
    }
  }

  .bulk-actions {
    display: flex;
    gap: 0.5rem;
    flex-shrink: 0;
  }

  .bulk-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 0.5rem 1rem;
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    font-size: 0.875rem;
    font-weight: 500;
    cursor: pointer;
  }

  .bulk-btn-primary {
    background-color: var(--text-primary);
    color: var(--color-primary);
  }

  .bulk-btn-secondary {
    background-color: var(--color-primary-hover);
    color: var(--text-primary);
  }

  .bulk-btn:hover {
    opacity: 0.9;
  }
</style>
