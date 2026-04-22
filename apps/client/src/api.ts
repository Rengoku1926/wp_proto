import { User, Group } from './types';

export async function listUsers(): Promise<User[]> {
  const res = await fetch('/api/users');
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function getUserByUsername(username: string): Promise<User> {
  const res = await fetch(`/api/users/search?username=${encodeURIComponent(username)}`);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function registerUser(username: string): Promise<User> {
  const res = await fetch('/api/users', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username }),
  });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function createGroup(name: string): Promise<Group> {
  const res = await fetch('/api/groups', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function addMember(groupId: string, userId: string): Promise<void> {
  const res = await fetch(`/api/groups/${groupId}/members`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ user_id: userId }),
  });
  if (!res.ok) throw new Error(await res.text());
}

export async function listMembers(groupId: string): Promise<{ group_id: string; user_id: string }[]> {
  const res = await fetch(`/api/groups/${groupId}/members`);
  if (!res.ok) return [];
  return res.json();
}
