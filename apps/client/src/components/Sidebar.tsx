import { useState } from 'react';
import { User, Group, Message, SelectedChat, dmKey, groupKey } from '../types';
import { listUsers } from '../api';
import ConversationItem from './ConversationItem';

interface Props {
  currentUser: User;
  users: User[];
  groups: Group[];
  connectedUsers: Set<string>;
  selectedChat: SelectedChat | null;
  messages: Record<string, Message[]>;
  activeTab: 'all' | 'personal' | 'groups';
  onTabChange: (tab: 'all' | 'personal' | 'groups') => void;
  onSelectChat: (chat: SelectedChat) => void;
  onConnect: (userId: string) => void;
  onDisconnect: (userId: string) => void;
  onAddUser: (user: User) => void;
  onCreateGroup: (name: string, memberIds: string[]) => Promise<Group>;
  onSignOut: () => void;
}

function initials(name: string) {
  return name.slice(0, 2).toUpperCase();
}

export default function Sidebar({
  currentUser, users, groups, connectedUsers, selectedChat,
  messages, activeTab, onTabChange, onSelectChat, onConnect,
  onDisconnect, onAddUser, onCreateGroup, onSignOut,
}: Props) {
  const [search, setSearch] = useState('');
  const [showNewChat, setShowNewChat] = useState(false);
  const [showAddGroup, setShowAddGroup] = useState(false);
  const [allUsers, setAllUsers] = useState<User[]>([]);
  const [userSearch, setUserSearch] = useState('');
  const [newGroupName, setNewGroupName] = useState('');
  const [selectedMembers, setSelectedMembers] = useState<string[]>([currentUser.id]);
  const [loading, setLoading] = useState(false);
  const [sessionsOpen, setSessionsOpen] = useState(true);

  const otherUsers = users.filter(u => u.id !== currentUser.id);

  const getLastMsg = (key: string) => {
    const msgs = messages[key] ?? [];
    return msgs[msgs.length - 1] ?? null;
  };

  const dmConvs = otherUsers.map(u => ({
    type: 'dm' as const,
    id: u.id,
    name: u.username,
    key: dmKey(currentUser.id, u.id),
    isOnline: connectedUsers.has(u.id),
  }));

  const groupConvs = groups.map(g => ({
    type: 'group' as const,
    id: g.id,
    name: g.name,
    key: groupKey(g.id),
    isOnline: false,
  }));

  const allConvs = [...dmConvs, ...groupConvs];

  const filtered = allConvs.filter(c => {
    if (search && !c.name.toLowerCase().includes(search.toLowerCase())) return false;
    if (activeTab === 'personal') return c.type === 'dm';
    if (activeTab === 'groups') return c.type === 'group';
    return true;
  });

  const openNewChat = async () => {
    setShowNewChat(true);
    setUserSearch('');
    try {
      const fetched = await listUsers();
      setAllUsers(fetched.filter(u => u.id !== currentUser.id));
    } catch { /* ignore */ }
  };

  const handleSelectNewChat = (user: User) => {
    onAddUser(user);
    onSelectChat({ type: 'dm', id: user.id, name: user.username });
    setShowNewChat(false);
  };

  const handleCreateGroup = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newGroupName.trim() || selectedMembers.length < 1) return;
    setLoading(true);
    try {
      await onCreateGroup(newGroupName.trim(), selectedMembers);
      setNewGroupName('');
      setSelectedMembers([currentUser.id]);
      setShowAddGroup(false);
    } catch { /* ignore */ }
    finally { setLoading(false); }
  };

  const toggleMember = (uid: string) => {
    if (uid === currentUser.id) return;
    setSelectedMembers(prev =>
      prev.includes(uid) ? prev.filter(id => id !== uid) : [...prev, uid]
    );
  };

  const filteredAllUsers = allUsers.filter(u =>
    !userSearch || u.username.toLowerCase().includes(userSearch.toLowerCase())
  );

  return (
    <div className="w-80 flex-shrink-0 bg-white flex flex-col border-r border-gray-100 h-screen">
      {/* Header */}
      <div className="px-4 pt-5 pb-3 flex-shrink-0">
        <div className="flex items-center justify-between mb-4">
          <div className="flex items-center gap-2.5">
            <div className="relative">
              <div className="w-9 h-9 rounded-full bg-indigo-100 flex items-center justify-center">
                <span className="text-xs font-semibold text-indigo-600">{initials(currentUser.username)}</span>
              </div>
              <div className={`absolute -bottom-0.5 -right-0.5 w-3 h-3 rounded-full border-2 border-white ${connectedUsers.has(currentUser.id) ? 'bg-emerald-400' : 'bg-gray-300'}`} />
            </div>
            <div className="text-left">
              <p className="text-sm font-semibold text-gray-900 leading-tight">{currentUser.username}</p>
              <p className="text-xs text-gray-400 leading-tight">
                {connectedUsers.has(currentUser.id) ? 'online' : 'offline'}
              </p>
            </div>
          </div>
          <div className="flex items-center gap-1">
            <button
              onClick={() => onConnect(currentUser.id)}
              title="Connect WS"
              className="p-1.5 rounded-lg text-gray-400 hover:text-emerald-500 hover:bg-emerald-50 transition-colors"
            >
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M13 10V3L4 14h7v7l9-11h-7z" />
              </svg>
            </button>
            <button
              onClick={() => { setShowNewChat(!showNewChat); setShowAddGroup(false); if (!showNewChat) openNewChat(); }}
              className="p-1.5 rounded-lg text-gray-400 hover:text-indigo-500 hover:bg-indigo-50 transition-colors"
              title="New chat"
            >
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M18 9v3m0 0v3m0-3h3m-3 0h-3m-2-5a4 4 0 11-8 0 4 4 0 018 0zM3 20a6 6 0 0112 0v1H3v-1z" />
              </svg>
            </button>
            <button
              onClick={() => { setShowAddGroup(!showAddGroup); setShowNewChat(false); }}
              className="p-1.5 rounded-lg text-gray-400 hover:text-indigo-500 hover:bg-indigo-50 transition-colors"
              title="Create group"
            >
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0z" />
              </svg>
            </button>
            <button
              onClick={onSignOut}
              title="Sign out"
              className="p-1.5 rounded-lg text-gray-400 hover:text-red-500 hover:bg-red-50 transition-colors"
            >
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M17 16l4-4m0 0l-4-4m4 4H7m6 4v1a3 3 0 01-3 3H6a3 3 0 01-3-3V7a3 3 0 013-3h4a3 3 0 013 3v1" />
              </svg>
            </button>
          </div>
        </div>

        {/* New chat panel */}
        {showNewChat && (
          <div className="mb-3 animate-fade-in bg-gray-50 rounded-xl p-3 space-y-2">
            <p className="text-xs font-medium text-gray-500">Start a new conversation</p>
            <input
              autoFocus
              value={userSearch}
              onChange={e => setUserSearch(e.target.value)}
              placeholder="Search users…"
              className="w-full px-3 py-1.5 rounded-lg border border-gray-200 text-sm focus:outline-none focus:ring-2 focus:ring-indigo-400 bg-white"
            />
            <div className="space-y-1 max-h-48 overflow-y-auto">
              {filteredAllUsers.length === 0 ? (
                <p className="text-xs text-gray-400 text-center py-3">No users found</p>
              ) : (
                filteredAllUsers.map(u => (
                  <button
                    key={u.id}
                    onClick={() => handleSelectNewChat(u)}
                    className="w-full flex items-center gap-2.5 px-2 py-2 rounded-lg hover:bg-indigo-50 transition-colors text-left group"
                  >
                    <div className="w-7 h-7 rounded-full bg-indigo-100 flex items-center justify-center flex-shrink-0">
                      <span className="text-[10px] font-semibold text-indigo-600">{initials(u.username)}</span>
                    </div>
                    <span className="text-sm text-gray-700 group-hover:text-indigo-700 font-medium">{u.username}</span>
                    {connectedUsers.has(u.id) && (
                      <span className="ml-auto w-2 h-2 rounded-full bg-emerald-400 flex-shrink-0" />
                    )}
                  </button>
                ))
              )}
            </div>
          </div>
        )}

        {showAddGroup && (
          <div className="mb-3 animate-fade-in bg-gray-50 rounded-xl p-3 space-y-2">
            <form onSubmit={handleCreateGroup} className="space-y-2">
              <input
                autoFocus
                value={newGroupName}
                onChange={e => setNewGroupName(e.target.value)}
                placeholder="Group name"
                className="w-full px-3 py-1.5 rounded-lg border border-gray-200 text-sm focus:outline-none focus:ring-2 focus:ring-indigo-400"
              />
              <div className="space-y-1">
                <p className="text-xs text-gray-500 font-medium">Members</p>
                <div className="flex flex-wrap gap-1">
                  {users.map(u => (
                    <button
                      key={u.id}
                      type="button"
                      onClick={() => toggleMember(u.id)}
                      className={`px-2 py-0.5 rounded-full text-xs font-medium transition-colors ${
                        selectedMembers.includes(u.id)
                          ? 'bg-indigo-600 text-white'
                          : 'bg-white border border-gray-200 text-gray-600'
                      } ${u.id === currentUser.id ? 'opacity-50 cursor-default' : ''}`}
                    >
                      {u.username}
                    </button>
                  ))}
                </div>
              </div>
              <button
                type="submit"
                disabled={loading || !newGroupName.trim()}
                className="w-full py-1.5 rounded-lg bg-indigo-600 text-white text-xs font-medium disabled:opacity-40"
              >
                Create Group
              </button>
            </form>
          </div>
        )}

        {/* Search */}
        <div className="relative">
          <svg className="absolute left-3 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
          </svg>
          <input
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="Search conversations"
            className="w-full pl-8 pr-3 py-2 rounded-xl bg-gray-100 text-sm text-gray-700 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-indigo-400 focus:bg-white transition"
          />
        </div>

        {/* Tabs */}
        <div className="flex gap-1 mt-3 bg-gray-100 rounded-xl p-1">
          {(['all', 'personal', 'groups'] as const).map(tab => (
            <button
              key={tab}
              onClick={() => onTabChange(tab)}
              className={`flex-1 py-1.5 rounded-lg text-xs font-medium transition-all ${
                activeTab === tab
                  ? 'bg-white text-gray-900 shadow-sm'
                  : 'text-gray-500 hover:text-gray-700'
              }`}
            >
              {tab.charAt(0).toUpperCase() + tab.slice(1)}
            </button>
          ))}
        </div>
      </div>

      {/* Conversation list */}
      <div className="flex-1 overflow-y-auto px-2 pb-2">
        {filtered.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-12 text-center px-4">
            <div className="w-12 h-12 rounded-full bg-gray-100 flex items-center justify-center mb-3">
              <svg className="w-5 h-5 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
              </svg>
            </div>
            <p className="text-sm text-gray-400">No conversations yet</p>
            <p className="text-xs text-gray-300 mt-1">Click the person+ icon to start chatting</p>
          </div>
        ) : (
          filtered.map(conv => (
            <ConversationItem
              key={`${conv.type}:${conv.id}`}
              type={conv.type}
              id={conv.id}
              name={conv.name}
              isOnline={conv.isOnline}
              isSelected={selectedChat?.type === conv.type && selectedChat?.id === conv.id}
              lastMessage={getLastMsg(conv.key)}
              currentUserId={currentUser.id}
              onClick={() => onSelectChat({ type: conv.type, id: conv.id, name: conv.name })}
            />
          ))
        )}
      </div>

      {/* WS Sessions panel */}
      <div className="flex-shrink-0 border-t border-gray-100">
        <button
          onClick={() => setSessionsOpen(o => !o)}
          className="w-full flex items-center justify-between px-4 py-3 text-xs font-medium text-gray-500 hover:text-gray-700 hover:bg-gray-50 transition-colors"
        >
          <span className="uppercase tracking-wider">WS Sessions</span>
          <svg className={`w-3.5 h-3.5 transition-transform ${sessionsOpen ? 'rotate-180' : ''}`} fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M19 9l-7 7-7-7" />
          </svg>
        </button>
        {sessionsOpen && (
          <div className="px-2 pb-3 space-y-1 max-h-40 overflow-y-auto">
            {users.map(user => {
              const online = connectedUsers.has(user.id);
              return (
                <div key={user.id} className="flex items-center gap-2 px-2 py-1.5 rounded-lg hover:bg-gray-50">
                  <div className={`w-2 h-2 rounded-full flex-shrink-0 ${online ? 'bg-emerald-400' : 'bg-gray-300'}`} />
                  <span className="flex-1 text-xs text-gray-700 truncate font-medium">{user.username}</span>
                  {user.id === currentUser.id && (
                    <span className="text-[10px] text-indigo-400 font-medium">you</span>
                  )}
                  <button
                    onClick={() => online ? onDisconnect(user.id) : onConnect(user.id)}
                    className={`text-[10px] font-medium px-2 py-0.5 rounded-full transition-colors ${
                      online
                        ? 'bg-red-50 text-red-500 hover:bg-red-100'
                        : 'bg-emerald-50 text-emerald-600 hover:bg-emerald-100'
                    }`}
                  >
                    {online ? 'Disconnect' : 'Connect'}
                  </button>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
