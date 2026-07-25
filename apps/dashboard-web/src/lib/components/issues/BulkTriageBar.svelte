<script lang="ts">
  export let selectedCount = 0;
  export let onResolve: () => void = () => {};
  export let onIgnore: () => void = () => {};
</script>

{#if selectedCount > 0}
<div class="fixed bottom-0 inset-x-0 pb-2 sm:pb-5 z-50">
  <div class="max-w-7xl mx-auto px-2 sm:px-6 lg:px-8">
    <div class="p-2 rounded-lg bg-indigo-600 shadow-lg sm:p-3">
      <div class="flex items-center justify-between flex-wrap">
        <div class="w-0 flex-1 flex items-center">
          <span class="flex p-2 rounded-lg bg-indigo-800">
            <svg class="h-6 w-6 text-white" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
          </span>
          <p class="ml-3 font-medium text-white truncate">
            <span class="md:hidden">{selectedCount} selected</span>
            <span class="hidden md:inline">{selectedCount} issues selected for bulk action</span>
          </p>
        </div>
        <div class="order-3 mt-2 flex-shrink-0 w-full sm:order-2 sm:mt-0 sm:w-auto flex space-x-2">
          <button on:click={onResolve} class="flex items-center justify-center px-4 py-2 border border-transparent rounded-md shadow-sm text-sm font-medium text-indigo-600 bg-white hover:bg-indigo-50">
            Mark Resolved
          </button>
          <button on:click={onIgnore} class="flex items-center justify-center px-4 py-2 border border-transparent rounded-md shadow-sm text-sm font-medium text-white bg-indigo-500 hover:bg-indigo-400">
            Ignore
          </button>
        </div>
      </div>
    </div>
  </div>
</div>
{/if}
