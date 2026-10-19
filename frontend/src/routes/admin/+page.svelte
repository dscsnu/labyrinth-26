<script lang="ts">
    import { enhance, type SubmitFunction } from "$app/forms";
    import type { Team } from "#lib/types/Team.type.ts";
    import type { PageProps } from "./$types";

    let { data, form }: PageProps = $props();
    let { user, teams } = $derived(data);

    let filter = $state("");
    let shown = $derived(
        teams.filter((t) => {
            const q = filter.trim().toLowerCase();
            return !q || t.id.includes(q) || t.name.toLowerCase().includes(q)
                || t.members.some((m) => m.name.toLowerCase().includes(q));
        }),
    );

    const nameOf = (t: Team, id: string) => t.members.find((m) => m.id === id)?.name ?? id.slice(0, 8);

    // Asks before removing (enhance's cancel stops the request); the backend
    // refuses anything invalid anyway. On success the page's data reloads.
    const confirmRemove = (t: Team, memberId: string): SubmitFunction => ({ cancel }) => {
        const leaderNote = memberId === t.leader_id ? " They lead the team; the chosen member takes over." : "";
        if (!confirm(`Remove ${nameOf(t, memberId)} from ${t.name} (${t.id}, ${t.status})?${leaderNote}`)) cancel();
    };
</script>

<p><a href="/">home</a> · {user?.name} · admin</p>

<h1>Teams</h1>

{#if form?.code}
    <p style="color: red">{form.code}: {form.message}</p>
{:else if form?.removed}
    <p style="color: green">Removed. They're teamless and can create or join a team.</p>
{/if}

<p>
    <input bind:value={filter} placeholder="filter by code, team or player" />
    {shown.length} of {teams.length} teams
</p>

{#each shown as t (t.id)}
    <section>
        <h2>{t.name} <small>({t.id} · {t.status} · {t.members.length} members)</small></h2>
        <ul>
            {#each t.members as m (m.id)}
                <li>
                    {m.name}{m.id === t.leader_id ? " (leader)" : ""}
                    {#if t.members.length === 1}
                        <small>— only member, can't be removed</small>
                    {:else}
                        <form method="POST" action="?/remove" use:enhance={confirmRemove(t, m.id)} style="display: inline">
                            <input type="hidden" name="team_id" value={t.id} />
                            <input type="hidden" name="user_id" value={m.id} />
                            {#if m.id === t.leader_id}
                                <label>
                                    new leader
                                    <select name="new_leader" required>
                                        <option value="" disabled selected>pick a member</option>
                                        {#each t.members.filter((o) => o.id !== m.id) as o (o.id)}
                                            <option value={o.id}>{o.name}</option>
                                        {/each}
                                    </select>
                                </label>
                            {/if}
                            <button>remove</button>
                        </form>
                    {/if}
                </li>
            {/each}
        </ul>
    </section>
{:else}
    <p>No teams.</p>
{/each}
