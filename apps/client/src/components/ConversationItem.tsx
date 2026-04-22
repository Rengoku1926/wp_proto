import { Message, DeliveryState } from '../types';

interface Props {
  type: 'dm' | 'group';
  id: string;
  name: string;
  isOnline: boolean;
  isSelected: boolean;
  lastMessage: Message | null;
  currentUserId: string;
  onClick: () => void;
}

function initials(name: string) {
  return name.slice(0, 2).toUpperCase();
}

function formatTime(d: Date) {
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function StateIcon({ state }: { state: DeliveryState }) {
  if (state === 0) return <span className="text-gray-300 text-xs">◷</span>;
  if (state === 1) return (
    <svg className="w-3.5 h-3.5 text-gray-400 inline" viewBox="0 0 16 16" fill="currentColor">
      <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
    </svg>
  );
  if (state === 2) return (
    <svg className="w-4 h-3.5 text-gray-400 inline" viewBox="0 0 20 16" fill="currentColor">
      <path d="M0.854 8.646a.5.5 0 010 .708l-4 4a.5.5 0 01-.708-.708L-.147 9H-3.5a.5.5 0 010-1h3.354l.646-.646a.5.5 0 01.708 0zM6.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L-.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
    </svg>
  );
  if (state >= 3) return (
    <span className="inline-flex">
      <svg className="w-3.5 h-3.5 text-indigo-500 inline" viewBox="0 0 16 16" fill="currentColor">
        <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      </svg>
      <svg className="w-3.5 h-3.5 text-indigo-500 inline -ml-1" viewBox="0 0 16 16" fill="currentColor">
        <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      </svg>
    </span>
  );
  return null;
}

export default function ConversationItem({
  type, name, isOnline, isSelected, lastMessage, currentUserId, onClick,
}: Props) {
  const isMine = lastMessage?.senderId === currentUserId;

  return (
    <button
      onClick={onClick}
      className={`w-full flex items-center gap-3 px-3 py-2.5 rounded-xl transition-all text-left ${
        isSelected ? 'bg-indigo-50' : 'hover:bg-gray-50'
      }`}
    >
      {/* Avatar */}
      <div className="relative flex-shrink-0">
        <div className={`w-10 h-10 rounded-full flex items-center justify-center text-xs font-semibold ${
          type === 'group' ? 'bg-purple-100 text-purple-600' : 'bg-indigo-100 text-indigo-600'
        }`}>
          {initials(name)}
        </div>
        {type === 'dm' && (
          <div className={`absolute -bottom-0.5 -right-0.5 w-3 h-3 rounded-full border-2 border-white ${isOnline ? 'bg-emerald-400' : 'bg-gray-300'}`} />
        )}
        {type === 'group' && (
          <div className="absolute -bottom-0.5 -right-0.5 w-3.5 h-3.5 rounded-full border-2 border-white bg-purple-500 flex items-center justify-center">
            <svg className="w-2 h-2 text-white" fill="currentColor" viewBox="0 0 20 20">
              <path d="M13 6a3 3 0 11-6 0 3 3 0 016 0zM18 8a2 2 0 11-4 0 2 2 0 014 0zM14 15a4 4 0 00-8 0v3h8v-3zM6 8a2 2 0 11-4 0 2 2 0 014 0zM16 18v-3a5.972 5.972 0 00-.75-2.906A3.005 3.005 0 0119 15v3h-3zM4.75 12.094A5.973 5.973 0 004 15v3H1v-3a3 3 0 013.75-2.906z" />
            </svg>
          </div>
        )}
      </div>

      {/* Content */}
      <div className="flex-1 min-w-0">
        <div className="flex items-center justify-between">
          <p className={`text-sm font-medium truncate ${isSelected ? 'text-indigo-700' : 'text-gray-900'}`}>{name}</p>
          {lastMessage && (
            <span className="text-[10px] text-gray-400 flex-shrink-0 ml-2">
              {formatTime(new Date(lastMessage.timestamp))}
            </span>
          )}
        </div>
        <div className="flex items-center gap-1 mt-0.5">
          {lastMessage ? (
            <>
              {isMine && <StateIcon state={lastMessage.state} />}
              <p className="text-xs text-gray-500 truncate">
                {isMine ? '' : ''}{lastMessage.content}
              </p>
            </>
          ) : (
            <p className="text-xs text-gray-300 italic">No messages yet</p>
          )}
        </div>
      </div>
    </button>
  );
}
