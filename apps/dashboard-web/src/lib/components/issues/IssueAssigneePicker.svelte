<script lang="ts">
  export let assignee: { type: 'user' | 'agent', id: string, name: string, avatarUrl?: string } | null = null;
  export let onAssign: (assignee: any) => void = () => {};

  let isOpen = false;
  
  function toggle() {
    isOpen = !isOpen;
  }
</script>

<div class="relative inline-block text-left">
  <button type="button" class="inline-flex justify-center w-full rounded-md border border-gray-300 shadow-sm px-4 py-2 bg-white text-sm font-medium text-gray-700 hover:bg-gray-50 focus:outline-none" on:click={toggle}>
    {#if assignee}
      {#if assignee.type === 'agent'}
        🤖 {assignee.name}
      {:else}
        👤 {assignee.name}
      {/if}
    {:else}
      Unassigned
    {/if}
  </button>

  {#if isOpen}
    <div class="origin-top-right absolute right-0 mt-2 w-56 rounded-md shadow-lg bg-white ring-1 ring-black ring-opacity-5 focus:outline-none z-10">
      <div class="py-1">
        <button class="text-gray-700 block w-full text-left px-4 py-2 text-sm hover:bg-gray-100" on:click={() => { onAssign(null); isOpen = false; }}>Unassigned</button>
        <button class="text-gray-700 block w-full text-left px-4 py-2 text-sm hover:bg-gray-100" on:click={() => { onAssign({ type: 'user', id: '1', name: 'Alice' }); isOpen = false; }}>👤 Alice</button>
        <button class="text-gray-700 block w-full text-left px-4 py-2 text-sm hover:bg-gray-100" on:click={() => { onAssign({ type: 'agent', id: '2', name: 'AutoFix Agent' }); isOpen = false; }}>🤖 AutoFix Agent</button>
      </div>
    </div>
  {/if}
</div>
