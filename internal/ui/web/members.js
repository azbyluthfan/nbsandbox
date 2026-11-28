// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// A space's members (docs/plan-shared-spaces.md): everyone sees who has
// which role; owners grant, change and revoke; anyone can leave; the
// space's creator can hand it over to another owner.

import { el, clear, toast, timeAgo, icon } from "./dom.js";

export const ROLES = {
  read: "read: sees files and runs, calls functions",
  write: "write: also edits files, runs, opens sessions, publishes",
  owner: "owner: also members, names, renaming and deleting",
};

/** Whether role is need or above. */
export function allows(role, need) {
  const rank = { read: 1, write: 2, owner: 3 };
  return (rank[role] || 0) >= (rank[need] || 99);
}

/**
 * The Members section of a space's page. me is the caller's ID, role
 * their role in the space; done(left) runs after a change (left: they
 * are no longer a member).
 */
export function membersSection(api, space, me, role, done) {
  const section = el("section", { class: "section members", hidden: true });
  const owner = allows(role, "owner");
  const draw = (members) => {
    const creator = members.find((m) => m.creator);
    const rows = members.map((m) => {
      const roleCell = owner && !m.creator
        ? el("select", { "aria-label": `${m.principal}'s role`, onchange: (e) => change(m.principal, e.target.value) },
          Object.keys(ROLES).map((r) => el("option", { value: r, selected: r === m.role, title: ROLES[r] }, r)))
        : el("span", { class: "badge-role" }, m.role);
      const actions = [];
      if (!m.creator && (owner || m.principal === me)) {
        actions.push(el("button", { type: "button", class: "ghost small", title: m.principal === me ? "Leave this space" : `Remove ${m.principal}`,
          onclick: () => remove(m.principal) }, icon(m.principal === me ? "x" : "trash")));
      }
      if (creator && creator.principal === me && m.role === "owner" && !m.creator) {
        actions.push(el("button", { type: "button", class: "ghost small", title: `Hand the space over to ${m.principal}`,
          onclick: () => handOver(m.principal) }, icon("chevron-right")));
      }
      return el("tr", {},
        el("td", {}, el("code", {}, m.principal), m.principal === me ? el("span", { class: "muted" }, " (you)") : null),
        el("td", {}, roleCell, m.creator ? el("span", { class: "muted small" }, " creator") : null),
        el("td", { class: "muted hide-narrow" }, m.creator ? "" : `by ${m.granted_by}, ${timeAgo(m.granted_at)}`),
        el("td", {}, actions));
    });
    const who = el("input", { "aria-label": "Caller ID", placeholder: "a caller ID: a username, or prefix:subject for a token", autocomplete: "off" });
    const pick = el("select", { "aria-label": "Role" }, Object.keys(ROLES).map((r) => el("option", { value: r, title: ROLES[r] }, r)));
    const grant = el("button", { type: "button" }, icon("plus"), "Grant");
    grant.addEventListener("click", () => {
      const p = who.value.trim();
      if (p) change(p, pick.value);
    });
    clear(section).append(
      el("div", { class: "section-head" }, el("h2", {}, "Members"),
        el("span", { class: "muted" }, members.length > 1 ? "who works in this space" : "only you, so far")),
      el("div", { class: "card table-card" }, el("div", { class: "scroll-x" }, el("table", { class: "list" }, el("tbody", {}, rows))),
        owner ? el("div", { class: "actions grant" }, who, pick, grant) : null,
        owner ? el("p", { class: "muted small" },
          "Each member reads data with their own connections, under this space's names; runs count toward whoever starts them.") : null));
    section.hidden = false;
  };
  const load = async () => {
    try {
      draw(await api.members(space));
    } catch (e) {
      toast(e.message);
    }
  };
  const change = async (principal, r) => {
    try {
      await api.putMember(space, principal, r);
      toast(`${principal}: ${r}.`, "info");
      load();
      done(false);
    } catch (e) {
      toast(e.message);
      load();
    }
  };
  const remove = async (principal) => {
    const leaving = principal === me;
    if (!confirm(leaving ? "Leave this space? You'll no longer see it." : `Remove ${principal} from this space?`)) return;
    try {
      await api.deleteMember(space, principal);
      if (leaving) return done(true);
      load();
    } catch (e) {
      toast(e.message);
    }
  };
  const handOver = async (to) => {
    if (!confirm(`Hand this space over to ${to}? Its files will count toward their quotas; you stay an owner.`)) return;
    try {
      await api.transfer(space, to);
      toast(`${to} is this space's creator now.`, "info");
      load();
      done(false);
    } catch (e) {
      toast(e.message);
    }
  };
  load();
  return section;
}
