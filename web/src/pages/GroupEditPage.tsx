import * as React from "react";
import i18next from "i18next";
import {Link, useLocation, useParams} from "react-router-dom";
import {MapTable} from "@/components/crud/MapTable";
import {Badge} from "@/components/ui/badge";
import {SimpleEditPage, type EditField} from "@/components/crud/SimpleEditPage";
import {useAccount} from "@/hooks/use-account";
import {useGroupOptions, useOrganizationOptions, useUserOptions} from "@/hooks/use-options";
import * as GroupBackend from "@/backend/GroupBackend";
import * as Setting from "@/lib/setting";

export default function GroupEditPage() {
  const {organizationName = "", groupName = ""} = useParams();
  const {account} = useAccount();
  const isGroupManager = Setting.isScopedAdmin(account) && !Setting.isLocalAdminUser(account);
  const organizations = useOrganizationOptions();
  // The Organization dropdown decides which users and groups this page offers.
  // It is tracked separately from the URL param because in "add" mode the user
  // can change the organization after the record was created, and the
  // admin-users and parent-group pickers (not just the URL) must follow that
  // selection; otherwise a group in one org would offer the users of another.
  const [owner, setOwner] = React.useState(organizationName);
  const groups = useGroupOptions(owner);
  const userList = useUserOptions(owner);
  const location = useLocation();
  // the group tree hands over where to return to; kept in a ref because "Save"
  // re-navigates to the saved name and drops the router state
  const backTo = React.useRef<string>((location.state as {backTo?: string} | null)?.backTo ?? "/groups");

  const fields: EditField[] = [
    {
      type: "select",
      name: "owner",
      labelKey: "general:Organization",
      options: () => organizations,
      disabled: () => !Setting.isAdminUser(account),
      // keep the pickers in sync with the organization selected in the form
      onChange: (value, _ctx, updateFields) => {
        updateFields({owner: value});
        setOwner(value);
      },
    },
    {type: "text", name: "name", labelKey: "general:Name", required: true, disabled: () => isGroupManager},
    {type: "text", name: "displayName", labelKey: "general:Display name"},
    {
      type: "select",
      name: "type",
      labelKey: "general:Type",
      options: () => [
        {value: "Virtual", label: i18next.t("group:Virtual")},
        {value: "Physical", label: i18next.t("group:Physical")},
      ],
    },
    {
      type: "select",
      name: "parentId",
      labelKey: "group:Parent group",
      disabled: () => isGroupManager,
      options: (ctx) => [
        {value: ctx.record.owner, label: ctx.record.owner},
        ...groups
          .filter((option) => option.value !== `${ctx.record.owner}/${ctx.record.name}`)
          .map((option) => ({...option, value: option.value.split("/")[1]})),
      ],
    },
    {
      // read-only: membership is edited from the user page, as in the antd frontend
      type: "custom",
      name: "users",
      labelKey: "general:Users",
      render: (ctx) => {
        const users: string[] = ctx.record.users ?? [];
        if (users.length === 0) {
          return <span className="text-sm text-muted-foreground">-</span>;
        }
        return (
          <div className="flex flex-wrap gap-1">
            {users.map((user) => (
              <Link key={user} to={`/users/${user}`}>
                <Badge variant="secondary" className="font-normal hover:bg-secondary/70">{user}</Badge>
              </Link>
            ))}
          </div>
        );
      },
    },
    {
      type: "multiselect",
      name: "adminUsers",
      labelKey: "group:Group admin users",
      options: () => userList,
      creatable: true,
      placeholder: i18next.t("group:Select admin users for this group"),
    },
    {type: "text", name: "contactEmail", labelKey: "general:Email"},
    {type: "number", name: "gidNumber", labelKey: "general:GID number"},
    {
      type: "custom",
      name: "properties",
      labelKey: "user:Properties",
      block: true,
      render: (ctx, update) => (
        <MapTable
          value={ctx.record.properties}
          onChange={(value) => update("properties", value)}
          keyTitle={i18next.t("general:Name")}
          valueTitle={i18next.t("webhook:Value")}
          keyWidth={240}
        />
      ),
    },
    {type: "switch", name: "isEnabled", labelKey: "general:Is enabled"},
  ];

  return (
    <SimpleEditPage
      titleKey="group:Edit Group"
      backTo={backTo.current}
      deps={[organizationName, groupName]}
      fields={fields}
      fetch={() => GroupBackend.getGroup(organizationName, groupName)}
      add={(record) => GroupBackend.addGroup(record)}
      update={(record) => GroupBackend.updateGroup(organizationName, groupName, record)}
      editUrl={(record) => `/groups/${record.owner}/${record.name}`}
    />
  );
}
